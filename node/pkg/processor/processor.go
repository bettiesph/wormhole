package processor

import (
	"context"
	"crypto/ecdsa"
	"time"

	"github.com/certusone/wormhole/node/pkg/notify/discord"

	"github.com/certusone/wormhole/node/pkg/db"
	"github.com/certusone/wormhole/node/pkg/governor"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"go.uber.org/zap"

	"github.com/certusone/wormhole/node/pkg/common"
	gossipv1 "github.com/certusone/wormhole/node/pkg/proto/gossip/v1"
	"github.com/certusone/wormhole/node/pkg/reporter"
	"github.com/certusone/wormhole/node/pkg/supervisor"
	"github.com/wormhole-foundation/wormhole/sdk/vaa"
)

type (
	// Observation defines the interface for any events observed by the guardian.
	Observation interface {
		// GetEmitterChain returns the id of the chain where this event was observed.
		GetEmitterChain() vaa.ChainID
		// MessageID returns a human-readable emitter_chain/emitter_address/sequence tuple.
		MessageID() string
		// SigningMsg returns the hash of the signing body of the observation. This is used
		// for signature generation and verification.
		SigningMsg() ethcommon.Hash
		// HandleQuorum finishes processing the observation once a quorum of signatures have
		// been received for it.
		HandleQuorum(sigs []*vaa.Signature, hash string, p *Processor)
	}

	// a Batch is a group of messages (Observations) emitted by a single transaction.
	Batch interface {
		// GetEmitterChain returns the id of the chain where this event was observed.
		GetEmitterChain() vaa.ChainID
		// TransactionID returns the unique identifer of the transaction from the source chain.
		GetTransactionID() ethcommon.Hash
		// BatchID returns a human-readable emitter_chain/transaction_id.
		BatchID() string
		// SigningMsg returns the hash of the signing body of the observation. This is used
		// for signature generation and verification.
		SigningBatchMsg() ethcommon.Hash
		// HandleQuorum finishes processing the observation once a quorum of signatures have
		// been received for it.
		HandleQuorum(sigs []*vaa.Signature, hash string, p *Processor)
	}

	// state represents the local view of a given observation
	state struct {
		// First time this digest was seen (possibly even before we observed it ourselves).
		firstObserved time.Time
		// Copy of our observation.
		ourObservation Observation
		// Map of signatures seen by guardian. During guardian set updates, this may contain signatures belonging
		// to either the old or new guardian set.
		signatures map[ethcommon.Address][]byte
		// Flag set after reaching quorum and submitting the VAA.
		submitted bool
		// Flag set by the cleanup service after the settlement timeout has expired and misses were counted.
		settled bool
		// Human-readable description of the VAA's source, used for metrics.
		source string
		// Number of times the cleanup service has attempted to retransmit this VAA.
		retryCount uint
		// Copy of the bytes we submitted (ourObservation, but signed and serialized). Used for retransmissions.
		ourMsg []byte
		// The hash of the transaction in which the observation was made.  Used for re-observation requests.
		txHash []byte
		// Copy of the guardian set valid at observation/injection time.
		gs *common.GuardianSet
	}

	batchState struct {
		state
		ourObservation Batch
	}

	observationMap map[string]*state

	// batchMap holds BatchMessages, keyed by BatchID
	batchMap map[*common.BatchMessageID]*common.BatchMessage

	// batchObservationMap tracks p2p gossip observations,
	// for progress toward BatchVAA quorum.
	batchObservationMap map[string]*batchState

	// observationsByTransactionID maps the transaction hash (batchID) to
	// to the messages within the transaction.
	// ie. { "BatchID": { "message_id": state } }
	observationsByTransactionID map[*common.BatchMessageID]map[string]*state // batchState

	// aggregationState represents the node's aggregation of guardian signatures.
	aggregationState struct {
		signatures observationMap

		transactions    observationsByTransactionID // collect messages by transactionID, to identify batch transactions.
		batches         batchMap                    // source of truth for which messages make up each batch.
		batchSignatures batchObservationMap         // collects signatures on a fully-observed batch of messages.
	}
)

type Processor struct {
	// lockC is a channel of observed emitted messages
	lockC chan *common.MessagePublication
	// setC is a channel of guardian set updates
	setC chan *common.GuardianSet

	// sendC is a channel of outbound messages to broadcast on p2p
	sendC chan []byte
	// obsvC is a channel of inbound decoded observations from p2p
	obsvC chan *gossipv1.SignedObservation

	// obsvReqSendC is a send-only channel of outbound re-observation requests to broadcast on p2p
	obsvReqSendC chan<- *gossipv1.ObservationRequest

	// signedInC is a channel of inbound signed VAA observations from p2p
	signedInC chan *gossipv1.SignedVAAWithQuorum

	// batchC is a ready-only channel of batched message data
	batchC chan *common.BatchMessage

	// batchReqC is a send-only channel for requesting batch message data
	batchReqC chan *common.BatchMessageID

	// batchObsvC is a channel of inbound decoded batch observations from p2p
	batchObsvC chan *gossipv1.SignedBatchObservation

	// batchInC is a channel of inbound signed Batch VAAs from p2p
	batchSignedInC chan *gossipv1.SignedBatchVAAWithQuorum

	// injectC is a channel of VAAs injected locally.
	injectC chan *vaa.VAA

	// gk is the node's guardian private key
	gk *ecdsa.PrivateKey

	// devnetMode specified whether to submit transactions to the hardcoded Ethereum devnet
	devnetMode         bool
	devnetNumGuardians uint
	devnetEthRPC       string

	attestationEvents *reporter.AttestationEventReporter

	logger *zap.Logger

	db *db.Database

	// Runtime state

	// gs is the currently valid guardian set
	gs *common.GuardianSet
	// gst is managed by the processor and allows concurrent access to the
	// guardian set by other components.
	gst *common.GuardianSetState

	// state is the current runtime VAA view
	state *aggregationState
	// gk pk as eth address
	ourAddr ethcommon.Address
	// cleanup triggers periodic state cleanup
	cleanup *time.Ticker

	notifier *discord.DiscordNotifier
	governor *governor.ChainGovernor
}

func NewProcessor(
	ctx context.Context,
	db *db.Database,
	lockC chan *common.MessagePublication,
	setC chan *common.GuardianSet,
	sendC chan []byte,
	obsvC chan *gossipv1.SignedObservation,
	obsvReqSendC chan<- *gossipv1.ObservationRequest,
	injectC chan *vaa.VAA,
	signedInC chan *gossipv1.SignedVAAWithQuorum,
	batchC chan *common.BatchMessage,
	batchReqC chan *common.BatchMessageID,
	batchObsvC chan *gossipv1.SignedBatchObservation,
	batchSignedInC chan *gossipv1.SignedBatchVAAWithQuorum,
	gk *ecdsa.PrivateKey,
	gst *common.GuardianSetState,
	devnetMode bool,
	devnetNumGuardians uint,
	devnetEthRPC string,
	attestationEvents *reporter.AttestationEventReporter,
	notifier *discord.DiscordNotifier,
	g *governor.ChainGovernor,
) *Processor {

	return &Processor{
		lockC:              lockC,
		setC:               setC,
		sendC:              sendC,
		obsvC:              obsvC,
		obsvReqSendC:       obsvReqSendC,
		signedInC:          signedInC,
		batchC:             batchC,
		batchReqC:          batchReqC,
		batchObsvC:         batchObsvC,
		batchSignedInC:     batchSignedInC,
		injectC:            injectC,
		gk:                 gk,
		gst:                gst,
		devnetMode:         devnetMode,
		devnetNumGuardians: devnetNumGuardians,
		devnetEthRPC:       devnetEthRPC,
		db:                 db,

		attestationEvents: attestationEvents,

		notifier: notifier,

		logger: supervisor.Logger(ctx),
		state: &aggregationState{
			observationMap{},
			observationsByTransactionID{},
			batchMap{},
			batchObservationMap{},
		},
		ourAddr:  crypto.PubkeyToAddress(gk.PublicKey),
		governor: g,
	}
}

func (p *Processor) Run(ctx context.Context) error {
	p.cleanup = time.NewTicker(30 * time.Second)

	// Always initialize the timer so don't have a nil pointer in the case below. It won't get rearmed after that.
	govTimer := time.NewTimer(time.Minute)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p.gs = <-p.setC:
			p.logger.Info("guardian set updated",
				zap.Strings("set", p.gs.KeysAsHexStrings()),
				zap.Uint32("index", p.gs.Index))
			p.gst.Set(p.gs)
		case k := <-p.lockC:
			if p.governor != nil {
				if !p.governor.ProcessMsg(k) {
					continue
				}
			}
			p.handleMessage(ctx, k)
		case v := <-p.injectC:
			p.handleInjection(ctx, v)
		case m := <-p.obsvC:
			p.handleObservation(ctx, m)
		case m := <-p.signedInC:
			p.handleInboundSignedVAAWithQuorum(ctx, m)
		case m := <-p.batchC:
			p.handleBatchMessage(ctx, m)
		case m := <-p.batchObsvC:
			p.handleBatchObservation(ctx, m)
		case m := <-p.batchSignedInC:
			p.handleInboundSignedBatchVAAWithQuorum(ctx, m)
		case <-p.cleanup.C:
			p.handleCleanup(ctx)
		case <-govTimer.C:
			if p.governor != nil {
				toBePublished, err := p.governor.CheckPending()
				if err != nil {
					return err
				}
				if len(toBePublished) != 0 {
					for _, k := range toBePublished {
						p.handleMessage(ctx, k)
					}
				}
				govTimer = time.NewTimer(time.Minute)
			}
		}
	}
}
