package gossip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/Fantom-foundation/lachesis-base/utils/datasemaphore"
	"github.com/Fantom-foundation/lachesis-base/utils/wlru"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover/discfilter"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"sonicpeer/pkg/gossip/topology"

	"github.com/0xsoniclabs/sonic/inter"
	"github.com/0xsoniclabs/sonic/logger"
	"github.com/0xsoniclabs/sonic/utils/caution"
)

const (
	softResponseLimitSize = 2 * 1024 * 1024    // Target maximum size of returned events, or other data.
	softLimitItems        = 250                // Target maximum number of events or transactions to request/response
	hardLimitItems        = softLimitItems * 4 // Maximum number of events or transactions to request/response

	TxTurnNonces = 32
)

func errResp(code errCode, format string, v ...interface{}) error {
	return fmt.Errorf("%v - %v", code, fmt.Sprintf(format, v...))
}

func checkLenLimits(size int, v interface{}) error {
	if size <= 0 {
		return errResp(ErrEmptyMessage, "%v", v)
	}
	if size > hardLimitItems {
		return errResp(ErrMsgTooLarge, "%v", v)
	}
	return nil
}

// handlerConfig holds the configuration for the  Handler.
type handlerConfig struct {
	dataDir string

	networkID uint64
	config    Config
	//s        *Store			// TODO: MAKE A STORE TSTRUCT TO HOLD ALL INFO FROM BACKEND AND SENTRY NODE
	genesis common.Hash
	url     string

	localId             enode.ID
	localEndPointSource LocalEndPointSource
	newTxsHook          newTxsHook
}

type LocalEndPointSource interface {
	GetLocalEndPoint() *enode.Node
}

// NewTxsHook is a function that gets a "first look" at a transaction.
// It should be zero-copy and non-blocking.
type newTxsHook func(tx *types.Transaction)

// handler is a lightweight P2P Handler that gossips transactions
// and events without maintaining local state or participating in consensus.
// It acts as a proxy for a trusted backend full node.
type handler struct {
	// ============= Sonic Node ================= //
	networkID uint64
	config    Config
	dataDir   string

	maxPeers int

	peers *peerSet

	msgSemaphore *datasemaphore.DataSemaphore

	//store    *Store 	// TODO: MAKE A STORE TSTRUCT TO HOLD ALL INFO FROM BACKEND AND SENTRY NODE

	// channels for syncer, txsyncLoop
	quitSync chan struct{}

	// wait group is used for graceful shutdowns during downloading
	// and processing
	loopsWg sync.WaitGroup
	wg      sync.WaitGroup
	peerWG  sync.WaitGroup
	started sync.WaitGroup

	peerInfoStop            chan<- struct{}
	peerInfoMaintenanceStop chan<- struct{}
	backendProgressStop     chan<- struct{}
	statsStop               chan<- struct{}
	// suggests new peers to connect to by monitoring the neighborhood
	connectionAdvisor topology.ConnectionAdvisor
	nextSuggestedPeer chan *enode.Node

	discoveredNodes   map[enode.ID]*enode.Node
	discoveredNodesMu sync.RWMutex

	localEndPointSource LocalEndPointSource
	newTxsHook          newTxsHook
	signer              types.Signer
	minGasPrice         *big.Int

	logger.Instance

	// ============= Sonicpeer ================= //
	genesis common.Hash
	backend *backendClient

	txHashCache  *wlru.Cache
	eventIDCache *wlru.Cache

	trackedEpoch atomic.Uint64
}

// newhandler creates a new  protocol Handler.
func newHandler(c handlerConfig) (*handler, error) {
	backend, err := newBackendClient(context.Background(), c.url)
	if err != nil {
		return nil, err
	}
	err = backend.Init()
	if err != nil {
		return nil, err
	}
	txHashCache, err := wlru.New(4*1024*1024, 4096) // 4MB, 1k items
	if err != nil {
		return nil, err
	}
	eventIDCache, err := wlru.New(4*1024*1024, 4096) // 4MB, 1k items
	if err != nil {
		return nil, err
	}

	h := &handler{
		networkID: c.networkID,
		config:    c.config,
		dataDir:   c.dataDir,

		msgSemaphore: datasemaphore.New(c.config.Protocol.MsgsSemaphoreLimit, getSemaphoreWarningFn("P2P messages")),
		//store:                c.s,
		peers:    newPeerSet(),
		quitSync: make(chan struct{}),

		connectionAdvisor: topology.NewConnectionAdvisor(c.localId),
		nextSuggestedPeer: make(chan *enode.Node, 1024),

		discoveredNodes: make(map[enode.ID]*enode.Node),

		localEndPointSource: c.localEndPointSource,
		newTxsHook:          c.newTxsHook,
		signer:              types.LatestSignerForChainID(new(big.Int).SetUint64(c.networkID)),
		minGasPrice:         c.config.GPO.MinGasPrice,

		Instance: logger.New("PM"),

		genesis: c.genesis,
		backend: backend,

		txHashCache:  txHashCache,
		eventIDCache: eventIDCache,
	}

	if epoch := backend.GetEpoch(); epoch != 0 {
		h.trackedEpoch.Store(uint64(epoch))
	}

	h.started.Add(1)
	return h, nil
}

func (h *handler) removePeer(id string) {
	peer := h.peers.Peer(id)
	if peer != nil {
		peer.Disconnect(p2p.DiscUselessPeer)
	}
}

func (h *handler) unregisterPeer(id string) {
	// Short circuit if the peer was already removed
	peer := h.peers.Peer(id)
	if peer == nil {
		return
	}
	log.Debug("Removing peer", "peer", id)

	// Unregister the peer from the peer sets
	if err := h.peers.UnregisterPeer(id); err != nil {
		log.Error("Peer removal failed", "peer", id, "err", err)
	}
}

// Start marks the Handler as started.
func (h *handler) Start(maxPeers int) {
	h.maxPeers = maxPeers
	h.Log.Info("Starting P2P Handler", "maxPeers", maxPeers)

	// Start the peerdiscovery
	h.loopsWg.Add(1)
	peerInfoStopChannel := make(chan struct{})
	h.peerInfoStop = peerInfoStopChannel
	go h.peerInfoCollectionLoop(peerInfoStopChannel)

	// Start the peer info maintenance loop (subset polling)
	h.loopsWg.Add(1)
	peerInfoMaintenanceStopChannel := make(chan struct{})
	h.peerInfoMaintenanceStop = peerInfoMaintenanceStopChannel
	go h.peerInfoMaintenanceLoop(peerInfoMaintenanceStopChannel)

	// Start the backend client loop
	h.loopsWg.Add(1)
	backendProgressStopChanel := make(chan struct{})
	h.backendProgressStop = backendProgressStopChanel
	go h.backend.NodeProgressLoop(backendProgressStopChanel, &h.loopsWg)

	// Start the stats loop
	h.loopsWg.Add(1)
	statsStopChannel := make(chan struct{})
	h.statsStop = statsStopChannel
	go h.statsLoop(statsStopChannel)

	// Load persisted peers
	h.loadPeers()

	h.started.Done()
}

// Stop terminates the Handler, disconnecting all peers.
func (h *handler) Stop() {
	log.Info("Stopping Sonic protocol")

	// Save peers to disk
	h.savePeers()

	// ======================== Sonic Original functionality ======================== //

	close(h.peerInfoStop)
	h.peerInfoStop = nil

	close(h.peerInfoMaintenanceStop)
	h.peerInfoMaintenanceStop = nil

	close(h.backendProgressStop)
	h.backendProgressStop = nil

	if h.statsStop != nil {
		close(h.statsStop)
		h.statsStop = nil
	}

	// Wait for the subscription loops to come down.
	h.loopsWg.Wait()

	// Quit the sync loop.
	// After this send has completed, no new peers will be accepted.
	close(h.quitSync)

	// Disconnect existing sessions.
	// This also closes the gate for any new registrations on the peer set.
	// sessions which are already established but not added to h.peers yet
	// will exit when they try to register.
	h.peers.Close()

	// Wait for all peer handler goroutines to come down.
	h.wg.Wait()
	h.peerWG.Wait()

	log.Info("Sonic protocol stopped")
}

func (h *handler) myProgress() PeerProgress {
	return h.backend.NodeProgress()
}

func (h *handler) checkNewEpoch() uint64 {
	currentEpoch := uint64(h.backend.GetEpoch())
	tracked := h.trackedEpoch.Load()

	if currentEpoch > tracked {
		if h.trackedEpoch.CompareAndSwap(tracked, currentEpoch) {
			// Apply exponential decay instead of wiping our memory
			log.Info("Epoch transition detected.", "new_epoch", currentEpoch)
		}
	}
	return currentEpoch
}

// PeerCount returns the number of currently connected peers.
func (h *handler) PeerCount() int {
	return h.peers.Len()
}

// NetworkID returns the network ID the handler is configured for.
func (h *handler) NetworkID() uint64 {
	return h.networkID
}

// isUseless checks if the peer is banned from discovery and ban it if it should be
func isUseless(node *enode.Node, name string) bool {
	return discfilter.Banned(node.ID(), node.Record())
}

// handle is the callback invoked to manage the life cycle of a peer.
func (h *handler) handle(p *peer) (err error) {
	p.Log().Trace("Connecting peer", "peer", p.ID(), "name", p.Name())

	useless := isUseless(p.Node(), p.Name())
	// For a Sentry node, we can be more permissive. We'll allow up to 50%
	// of our slots to be "useless" peers, as they still contribute to tx discovery.
	if !p.Peer.Info().Network.Trusted && useless && h.peers.UselessNum() >= h.maxPeers/2 {
		p.Log().Trace("Rejecting peer as useless", "peer", p.ID(), "name", p.Name())
		return p2p.DiscUselessPeer
	}
	if !p.Peer.Info().Network.Trusted && useless {
		p.SetUseless()
	}

	h.peerWG.Add(1)
	defer h.peerWG.Done()

	// Execute the handshake
	var (
		genesis    = h.genesis
		myProgress = h.myProgress()
	)

	if err := p.Handshake(h.networkID, myProgress, common.Hash(genesis)); err != nil {
		p.Log().Error("Handshake failed", "err", err, "peer", p.ID(), "name", p.Name())
		if !useless {
			discfilter.Ban(p.ID())
		}
		return err
	}

	// Ignore maxPeers if this is a trusted peer
	if h.peers.Len() >= h.maxPeers && !p.Peer.Info().Network.Trusted {
		p.Log().Trace("Rejecting peer as maxPeers is exceeded")
		return p2p.DiscTooManyPeers
	}
	p.Log().Info("Peer connected", "peer", p.ID(), "name", p.Name())

	// Register the peer locally
	if err := h.peers.RegisterPeer(p); err != nil {
		p.Log().Warn("Peer registration failed", "err", err)
		return err
	}
	defer h.unregisterPeer(p.id)

	if err := p.SendEndPointUpdateRequest(); err != nil {
		p.Log().Debug("Failed to send end-point update request", "err", err)
	}

	if err := p.SendPeerInfoRequest(); err != nil {
		p.Log().Debug("Failed to send initial peer info request", "err", err)
	}

	// Handle incoming messages until the connection is torn down
	for {
		if err := h.handleMsg(p); err != nil {
			level := slog.LevelWarn
			if errors.Is(err, io.EOF) {
				level = slog.LevelDebug
			}
			p.Log().Log(level, "Message handling failed", "err", err, "peer", p.ID(), "name", p.Name())
			return err
		}
	}
}

func (h *handler) handleTxHashes(p *peer, announces []common.Hash, receivedAt time.Time) {
	// Only request what we don't know
	toRequest := make([]common.Hash, 0, len(announces))

	// Mark the hashes as present at the remote node
	for _, id := range announces {
		// Mark transaction as seen for peer
		p.MarkTransaction(id)
		// Metric: Increment total_tx_announcements_received_total

		// Add it to the cache if its not found in the txHashCache and the txCache
		if !h.txHashCache.Contains(id) {
			h.txHashCache.Add(id, struct{}{}, 1)
			toRequest = append(toRequest, id)
		}
	}
	// Schedule all the unknown hashes for retrieval
	if err := p.RequestTransactions(toRequest); err != nil {
		p.Log().Warn("Failed to request transactions", "err", err)
	}
}

func (h *handler) handleTxs(p *peer, txs types.Transactions, receivedAt time.Time) {
	// Mark the hashes as present at the remote node
	for _, tx := range txs {
		id := tx.Hash()
		p.MarkTransaction(id)

		if !h.txHashCache.Contains(id) {
			// VALIDATION:
			// 1. Check if the gas price meets our minimum requirement to match
			// the mempool policy of a standard full node.
			if tx.GasPrice().Cmp(h.minGasPrice) < 0 {
				p.Log().Warn("Invalid transaction", "err", "gas price too low", "hash", id)
				continue
			}

			// 2. Verify the signature and ChainID. This filters out spam and
			// transactions from other networks.
			if _, err := types.Sender(h.signer, tx); err != nil {
				p.Log().Warn("Invalid transaction", "err", err, "hash", id)
				continue
			}

			h.txHashCache.Add(id, struct{}{}, 1)
			if h.newTxsHook != nil {
				h.newTxsHook(tx)
			}
		}
	}
}

func (h *handler) handleEventHashes(p *peer, announces hash.Events, receivedAt time.Time) {
	h.checkNewEpoch()

	// Only request what we don't know
	toRequest := make([]hash.Event, 0, len(announces))
	for _, id := range announces {
		p.MarkEvent(id)
		if !h.eventIDCache.Contains(id) {
			h.eventIDCache.Add(id, struct{}{}, 1)
			toRequest = append(toRequest, id)
		}
	}
	if err := p.RequestEvents(toRequest); err != nil {
		p.Log().Warn("Failed to request events", "err", err)
	}
}

func (h *handler) handleEvents(p *peer, events dag.Events, ordered bool, receivedAt time.Time) {
	h.checkNewEpoch()
	// Mark the hashes as present at the remote node
	for _, e := range events {
		id := e.ID()
		p.MarkEvent(id)

		// 2. Ignore old epoch events
		if e.Epoch() != h.backend.GetEpoch() {
			continue
		}

		// Standard Geth Cache logic continues...
		if !h.eventIDCache.Contains(id) {
			h.eventIDCache.Add(id, struct{}{}, 1)
		}
	}
	// Sonicpeer: Do nothing, cause we are not interrested in relaying events....
}

// handleMsg is the core message routing logic for the  sentry.
func (h *handler) handleMsg(p *peer) error {
	receivedAt := time.Now()

	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > protocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	defer caution.ExecuteAndReportError(&err, msg.Discard, "failed to discard message")

	// Acquire semaphore for serialized messages
	eventsSizeEst := dag.Metric{
		Num:  1,
		Size: uint64(msg.Size),
	}
	if !h.msgSemaphore.Acquire(eventsSizeEst, h.config.Protocol.MsgsSemaphoreTimeout) {
		h.Log.Warn("Failed to acquire semaphore for p2p message", "size", msg.Size, "peer", p.id)
		return nil
	}
	defer h.msgSemaphore.Release(eventsSizeEst)

	// Handle the message depending on its contents
	switch msg.Code {
	case HandshakeMsg:
		// Status messages should never arrive after the handshake
		return errResp(ErrExtraStatusMsg, "uncontrolled status message")

	case ProgressMsg:
		var progress PeerProgress
		if err := msg.Decode(&progress); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}

		if err := h.checkPeerStaleness(p, progress); err != nil {
			return err
		}
		p.SetProgress(progress)

	case EvmTxsMsg:
		// Transactions can be processed, parse all of them and deliver to the pool
		var txs types.Transactions
		if err := msg.Decode(&txs); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		if err := checkLenLimits(len(txs), txs); err != nil {
			return err
		}
		h.handleTxs(p, txs, receivedAt)

	case NewEvmTxHashesMsg:
		// Transactions can be processed, parse all of them and deliver to the pool
		var txHashes []common.Hash
		if err := msg.Decode(&txHashes); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		if err := checkLenLimits(len(txHashes), txHashes); err != nil {
			return err
		}
		h.handleTxHashes(p, txHashes, receivedAt)

	case GetEvmTxsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case EventsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case NewEventIDsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case GetEventsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case RequestEventsStream:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case EventsStreamResponse:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case GetPeerInfosMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case PeerInfosMsg:
		var infos peerInfoMsg
		if err := msg.Decode(&infos); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}

		reportedPeers := []*enode.Node{}
		for _, info := range infos.Peers {
			var enode enode.Node
			if err := enode.UnmarshalText([]byte(info.Enode)); err != nil {
				h.Log.Warn("Failed to unmarshal enode", "enode", info.Enode, "err", err)
			} else {
				reportedPeers = append(reportedPeers, &enode)
			}
		}

		h.discoveredNodesMu.Lock()
		for _, n := range reportedPeers {
			h.discoveredNodes[n.ID()] = n
		}
		h.discoveredNodesMu.Unlock()
		h.connectionAdvisor.UpdatePeers(p.ID(), reportedPeers)

	case GetEndPointMsg:
		source := h.localEndPointSource
		if source == nil {
			return nil
		}
		enode := source.GetLocalEndPoint()
		if enode == nil {
			return nil
		}
		if err := p2p.Send(p.rw, EndPointUpdateMsg, enode.String()); err != nil {
			return err
		}

	case EndPointUpdateMsg:
		var encoded string
		if err := msg.Decode(&encoded); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		var enode enode.Node
		if err := enode.UnmarshalText([]byte(encoded)); err != nil {
			h.Log.Warn("Failed to unmarshal enode", "enode", encoded, "err", err)
		} else {
			p.endPoint.Store(&peerEndPointInfo{
				enode:     enode,
				timestamp: time.Now(),
			})
		}

	default:
		return errResp(ErrInvalidMsgCode, "%v", msg.Code)
	}
	return nil
}

// checkPeerStaleness verifies if a peer's reported epoch matches the local epoch.
// If they don't match, it tracks the duration of the mismatch and disconnects
// the peer if it exceeds the configured grace period.
func (h *handler) checkPeerStaleness(p *peer, progress PeerProgress) error {
	currentEpoch := h.backend.GetEpoch()
	if progress.Epoch != currentEpoch {
		if h.config.Protocol.PeerStaleEpochGracePeriod > 0 {
			staleSince := p.staleSince
			if staleSince.IsZero() {
				p.Lock()
				if p.staleSince.IsZero() {
					p.staleSince = time.Now()
				}
				staleSince = p.staleSince
				p.Unlock()
			}

			staleDuration := time.Since(staleSince)
			if staleDuration > h.config.Protocol.PeerStaleEpochGracePeriod {
				p.Disconnect(p2p.DiscUselessPeer)
				return errResp(ErrUnSyncedPeer, "peer epoch %d != local epoch %d for %v", progress.Epoch, currentEpoch, staleDuration)
			}
		}
	} else if !p.staleSince.IsZero() {
		// Optimization: only lock if the peer was previously marked as stale.
		p.Lock()
		p.staleSince = time.Time{}
		p.Unlock()
	}
	return nil
}

func (h *handler) decideBroadcastAggressiveness(size int, passed time.Duration, peersNum int) int {
	percents := 100
	maxPercents := 1000000 * percents
	latencyVsThroughputTradeoff := maxPercents
	cfg := h.config.Protocol
	if cfg.ThroughputImportance != 0 {
		latencyVsThroughputTradeoff = (cfg.LatencyImportance * percents) / cfg.ThroughputImportance
	}

	broadcastCost := passed * time.Duration(128+size) / 128
	broadcastAllCostTarget := time.Duration(latencyVsThroughputTradeoff) * (700 * time.Millisecond) / time.Duration(percents)
	broadcastSqrtCostTarget := broadcastAllCostTarget * 10

	fullRecipients := 0
	if latencyVsThroughputTradeoff >= maxPercents {
		// edge case
		fullRecipients = peersNum
	} else if latencyVsThroughputTradeoff <= 0 {
		// edge case
		fullRecipients = 0
	} else if broadcastCost <= broadcastAllCostTarget {
		// if event is small or was created recently, always send to everyone full event
		fullRecipients = peersNum
	} else if broadcastCost <= broadcastSqrtCostTarget || passed == 0 {
		// if event is big but was created recently, send full event to subset of peers
		fullRecipients = int(math.Sqrt(float64(peersNum)))
		if fullRecipients < 4 {
			fullRecipients = 4
		}
	}
	if fullRecipients > peersNum {
		fullRecipients = peersNum
	}
	return fullRecipients
}

// BroadcastEvent will either propagate a event to a subset of it's peers, or
// will only announce it's availability (depending what's requested).
func (h *handler) BroadcastEvent(event *inter.EventPayload, passed time.Duration) int {
	if passed < 0 {
		passed = 0
	}
	id := event.ID()
	peers := h.peers.PeersWithoutEvent(id)
	if len(peers) == 0 {
		log.Trace("Event is already known to all peers", "hash", id)
		return 0
	}

	fullRecipients := h.decideBroadcastAggressiveness(event.Size(), passed, len(peers))

	// Exclude low quality peers from fullBroadcast
	var fullBroadcast = make([]*peer, 0, fullRecipients)
	var hashBroadcast = make([]*peer, 0, len(peers))
	for _, p := range peers {
		if !p.Useless() && len(fullBroadcast) < fullRecipients {
			fullBroadcast = append(fullBroadcast, p)
		} else {
			hashBroadcast = append(hashBroadcast, p)
		}
	}
	for _, peer := range fullBroadcast {
		peer.AsyncSendEvents(inter.EventPayloads{event}, peer.queue)
	}
	// Broadcast of event hash to the rest peers
	for _, peer := range hashBroadcast {
		peer.AsyncSendEventIDs(hash.Events{event.ID()}, peer.queue)
	}
	log.Trace("Broadcast event", "hash", id, "fullRecipients", len(fullBroadcast), "hashRecipients", len(hashBroadcast))
	return len(peers)
}

// BroadcastTxs will propagate a batch of transactions to all peers which are not known to
// already have the given transaction.
func (h *handler) BroadcastTxs(txs types.Transactions) {
	var txset = make(map[*peer]types.Transactions)

	// Broadcast transactions to a batch of peers not knowing about it
	totalSize := common.StorageSize(0)
	for _, tx := range txs {
		peers := h.peers.PeersWithoutTx(tx.Hash())
		for _, peer := range peers {
			txset[peer] = append(txset[peer], tx)
		}
		totalSize += common.StorageSize(tx.Size())
		log.Trace("Broadcast transaction", "hash", tx.Hash(), "recipients", len(peers))
	}
	fullRecipients := h.decideBroadcastAggressiveness(int(totalSize), time.Second, len(txset))
	i := 0
	for peer, txs := range txset {
		SplitTransactions(txs, func(batch types.Transactions) {
			if i < fullRecipients {
				peer.AsyncSendTransactions(batch, peer.queue)
			} else {
				txids := make([]common.Hash, batch.Len())
				for i, tx := range batch {
					txids[i] = tx.Hash()
				}
				peer.AsyncSendTransactionHashes(txids, peer.queue)
			}
		})
		i++
	}
}

func (h *handler) peerInfoCollectionLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(h.config.Protocol.PeerInfoCollectionPeriod)
	defer ticker.Stop()
	defer h.loopsWg.Done()

	collect := func() {
		// Get a suggestion for a new peer.
		// The number of suggestions to fetch is a trade-off between discovery speed and network load.
		// For a Sentry node focusing on connectivity, we increase this from 8 to 32
		// to ensure the P2P server always has a fresh pool of candidates to dial,
		// especially when the node is under-peered.
		suggested := make(map[string]struct{})
		for i := 0; i < 32; i++ {
			suggestion := h.connectionAdvisor.GetNewPeerSuggestion()
			if suggestion == nil {
				break
			}
			id := suggestion.ID().String()
			if _, ok := suggested[id]; ok {
				continue
			}
			// Only suggest if we aren't already connected to this peer
			if h.peers.Peer(id) == nil {
				select {
				case h.nextSuggestedPeer <- suggestion:
					suggested[id] = struct{}{}
				default:
					// Channel full
					goto EndSuggestions
				}
			}
		}
	EndSuggestions:

		// Request updated peer information from current peers.
		peers := h.peers.List()
		for _, peer := range peers {
			// If we do not have the peer's end-point or it is too old, request it.
			if info := peer.endPoint.Load(); info == nil || time.Since(info.timestamp) > h.config.Protocol.PeerEndPointUpdatePeriod {
				if err := peer.SendEndPointUpdateRequest(); err != nil {
					log.Warn("Failed to send end-point update request", "peer", peer.id, "err", err)
					// If the end-point update request fails, do not send the peer info request.
					continue
				}
			}
		}

		// Drop a redundant connection if there are too many connections.
		if len(suggested) > 0 && len(peers) >= h.maxPeers {
			redundant := h.connectionAdvisor.GetRedundantPeerSuggestion()
			if redundant != nil {
				for _, peer := range peers {
					if peer.Node().ID() == *redundant {
						peer.Disconnect(p2p.DiscTooManyPeers)
						break
					}
				}
			}
		}
	}

	// Run immediately to speed up initial discovery
	collect()

	for {
		select {
		case <-ticker.C:
			collect()
		case <-stop:
			return
		}
	}
}

func (h *handler) peerInfoMaintenanceLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(h.config.Protocol.PeerInfoMaintenancePeriod)
	defer ticker.Stop()
	defer h.loopsWg.Done()

	for {
		select {
		case <-ticker.C:
			peers := h.peers.List()
			if len(peers) == 0 {
				continue
			}

			// Shuffle to pick a random subset
			rand.Shuffle(len(peers), func(i, j int) {
				peers[i], peers[j] = peers[j], peers[i]
			})

			// Ask a small subset (e.g., up to 4 peers or 10% of the peerset)
			limit := 4
			if limit > len(peers) {
				limit = len(peers)
			}

			for i := 0; i < limit; i++ {
				if err := peers[i].SendPeerInfoRequest(); err != nil {
					log.Warn("Failed to send maintenance peer info request", "peer", peers[i].id, "err", err)
				}
			}
		case <-stop:
			return
		}
	}
}

func (h *handler) statsLoop(stop <-chan struct{}) {
	// Define the interval for logging stats. 15 seconds is a reasonable default.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	defer h.loopsWg.Done()

	for {
		select {
		case <-ticker.C:
			h.logPeerStats()
		case <-stop:
			return
		}
	}
}

func (h *handler) logPeerStats() {
	peers := h.peers.List()
	var inbound, outbound int
	for _, p := range peers {
		if p.Peer.Info().Network.Inbound {
			inbound++
		} else {
			outbound++
		}
	}

	h.Log.Info("Peer statistics",
		"connected", len(peers),
		"inbound", inbound,
		"outbound", outbound,
		"useless", h.peers.UselessNum(),
	)
}

type persistedPeer struct {
	Enode string `json:"enode"`
}

func (h *handler) savePeers() {
	if h.dataDir == "" {
		return
	}

	path := filepath.Join(h.dataDir, "nodes.json")

	h.discoveredNodesMu.RLock()
	defer h.discoveredNodesMu.RUnlock()

	// Create a unique set of all known nodes (connected + discovered via gossip)
	allNodes := make(map[enode.ID]*enode.Node)

	// 1. Add currently connected peers
	for _, p := range h.peers.List() {
		if n := p.Node(); n != nil {
			allNodes[n.ID()] = n
		}
	}
	// 2. Add all previously discovered nodes from the cache
	for id, n := range h.discoveredNodes {
		allNodes[id] = n
	}

	nodes := make([]persistedPeer, 0, len(allNodes))
	for _, n := range allNodes {
		nodes = append(nodes, persistedPeer{Enode: n.String()})
	}

	if len(nodes) == 0 {
		return
	}

	// Use MarshalIndent for better readability of the JSON file
	data, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		h.Log.Warn("Failed to marshal peers", "err", err)
		return
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		h.Log.Warn("Failed to save peers", "err", err)
	} else {
		h.Log.Info("Saved connected peers to disk", "count", len(nodes))
	}
}

func (h *handler) loadPeers() {
	if h.dataDir == "" {
		return
	}
	path := filepath.Join(h.dataDir, "nodes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var nodes []persistedPeer
	if err := json.Unmarshal(data, &nodes); err != nil {
		h.Log.Warn("Failed to unmarshal peers", "err", err)
		return
	}

	h.Log.Info("Loaded peers from disk", "count", len(nodes))

	go func() {
		for _, p := range nodes {
			n, err := enode.Parse(enode.ValidSchemes, p.Enode)
			if err != nil {
				continue
			}
			h.discoveredNodesMu.Lock()
			h.discoveredNodes[n.ID()] = n
			h.discoveredNodesMu.Unlock()

			select {
			case h.nextSuggestedPeer <- n:
			case <-h.quitSync:
				return
			}
		}
	}()
}

// NodeInfo represents a short summary of the sub-protocol metadata
// known about the host peer.
type NodeInfo struct {
	Network     uint64      `json:"network"` // network ID
	Genesis     common.Hash `json:"genesis"` // SHA3 hash of the host's genesis object
	Epoch       idx.Epoch   `json:"epoch"`
	NumOfBlocks idx.Block   `json:"blocks"`
	//Config  *params.ChainConfig `json:"config"`  // Chain configuration for the fork rules
}

// NodeInfo retrieves some protocol metadata about the running host node.
func (h *handler) NodeInfo() *NodeInfo {
	return &NodeInfo{
		Network:     h.networkID,
		Genesis:     h.genesis,
		Epoch:       h.backend.NodeProgress().Epoch,
		NumOfBlocks: h.backend.NodeProgress().LastBlockIdx,
	}
}

func getSemaphoreWarningFn(name string) func(dag.Metric, dag.Metric, dag.Metric) {
	return func(received dag.Metric, processing dag.Metric, releasing dag.Metric) {
		log.Warn(fmt.Sprintf("%s semaphore inconsistency", name),
			"receivedNum", received.Num, "receivedSize", received.Size,
			"processingNum", processing.Num, "processingSize", processing.Size,
			"releasingNum", releasing.Num, "releasingSize", releasing.Size)
	}
}

func (h *handler) GetSuggestedPeerIterator() enode.Iterator {
	return &suggestedPeerIterator{
		handler: h,
		close:   make(chan struct{}),
	}
}

type suggestedPeerIterator struct {
	handler *handler
	next    *enode.Node
	close   chan struct{}
}

func (i *suggestedPeerIterator) Next() bool {
	select {
	case i.next = <-i.handler.nextSuggestedPeer:
		return true
	case <-i.close:
		return false
	}
}

func (i *suggestedPeerIterator) Node() *enode.Node {
	return i.next
}

func (i *suggestedPeerIterator) Close() {
	close(i.close)
}
