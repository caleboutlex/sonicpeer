package gossip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Fantom-foundation/lachesis-base/gossip/dagprocessor"
	"github.com/Fantom-foundation/lachesis-base/gossip/itemsfetcher"
	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/Fantom-foundation/lachesis-base/utils/datasemaphore"
	"github.com/Fantom-foundation/lachesis-base/utils/wlru"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	notify "github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover/discfilter"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"

	"sonicpeer/pkg/gossip/topology"

	"github.com/0xsoniclabs/sonic/eventcheck"
	"github.com/0xsoniclabs/sonic/evmcore"
	"github.com/0xsoniclabs/sonic/gossip/protocols/dag/dagstream/dagstreamleecher"
	"github.com/0xsoniclabs/sonic/gossip/protocols/dag/dagstream/dagstreamseeder"
	"github.com/0xsoniclabs/sonic/inter"
	"github.com/0xsoniclabs/sonic/logger"
	"github.com/0xsoniclabs/sonic/utils/caution"
)

const (
	softResponseLimitSize = 2 * 1024 * 1024    // Target maximum size of returned events, or other data.
	softLimitItems        = 250                // Target maximum number of events or transactions to request/response
	hardLimitItems        = softLimitItems * 4 // Maximum number of events or transactions to request/response

	// txChanSize is the size of channel listening to NewTxsNotify.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	TxTurnNonces = 32
)

var txHashesBufPool = sync.Pool{
	New: func() interface{} {
		return make([]common.Hash, 0, 1024)
	},
}

var eventIDsBufPool = sync.Pool{
	New: func() interface{} {
		return make(hash.Events, 0, 1024)
	},
}

var eventBufPool = sync.Pool{
	New: func() interface{} {
		return make(inter.EventPayloads, 0, 1024)
	},
}

var txBufPool = sync.Pool{
	New: func() interface{} {
		return make(types.Transactions, 0, 1024)
	},
}

var peerSlicePool = sync.Pool{
	New: func() interface{} {
		return make([]*peer, 0, 128)
	},
}

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

type dagNotifier interface {
	SubscribeNewEpoch(ch chan<- idx.Epoch) notify.Subscription
	SubscribeNewEmitted(ch chan<- *inter.EventPayload) notify.Subscription
}

type processCallback struct {
	Event         func(*inter.EventPayload) error
	SwitchEpochTo func(idx.Epoch) error
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
	searcherHook        searcherHook
}

type LocalEndPointSource interface {
	GetLocalEndPoint() *enode.Node
}

// SearcherHook is a function that gets a "first look" at a transaction.
// It should be zero-copy and non-blocking.
type searcherHook func(tx *types.Transaction)

// handler is a lightweight P2P Handler that gossips transactions
// and events without maintaining local state or participating in consensus.
// It acts as a proxy for a trusted backend full node.
type handler struct {
	// ============= Sonic Node ================= //
	NetworkID uint64
	config    Config
	dataDir   string

	txpool   TxPool
	maxPeers int

	peers *peerSet

	txsCh  chan evmcore.NewTxsNotify
	txsSub notify.Subscription

	dagLeecher   *dagstreamleecher.Leecher
	dagSeeder    *dagstreamseeder.Seeder
	dagProcessor *dagprocessor.Processor
	dagFetcher   *itemsfetcher.Fetcher

	process processCallback

	txFetcher *itemsfetcher.Fetcher

	checkers *eventcheck.Checkers

	msgSemaphore *datasemaphore.DataSemaphore

	//store    *Store 	// TODO: MAKE A STORE TSTRUCT TO HOLD ALL INFO FROM BACKEND AND SENTRY NODE

	engineMu sync.Locker

	notifier             dagNotifier
	emittedEventsCh      chan *inter.EventPayload
	emittedEventsSub     notify.Subscription
	newEpochsCh          chan idx.Epoch
	newEpochsSub         notify.Subscription
	quitProgressBradcast chan struct{}

	// channels for syncer, txsyncLoop
	quitSync chan struct{}

	// wait group is used for graceful shutdowns during downloading
	// and processing
	loopsWg sync.WaitGroup
	wg      sync.WaitGroup
	peerWG  sync.WaitGroup
	started sync.WaitGroup

	peerInfoStop        chan<- struct{}
	backendProgressStop chan<- struct{}
	statsStop           chan<- struct{}
	// suggests new peers to connect to by monitoring the neighborhood
	connectionAdvisor topology.ConnectionAdvisor
	nextSuggestedPeer chan *enode.Node

	localEndPointSource LocalEndPointSource

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
		NetworkID: c.networkID,
		config:    c.config,
		dataDir:   c.dataDir,

		msgSemaphore: datasemaphore.New(c.config.Protocol.MsgsSemaphoreLimit, getSemaphoreWarningFn("P2P messages")),
		//store:                c.s,
		peers:    newPeerSet(),
		quitSync: make(chan struct{}),

		connectionAdvisor: topology.NewConnectionAdvisor(c.localId),
		nextSuggestedPeer: make(chan *enode.Node, 1024),

		localEndPointSource: c.localEndPointSource,

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

func (h *handler) peerMisbehaviour(peer string, err error) bool {
	if eventcheck.IsBan(err) {
		log.Warn("Dropping peer due to a misbehaviour", "peer", peer, "err", err)
		h.removePeer(peer)
		return true
	}
	return false
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

	// Start the backend client loop
	h.loopsWg.Add(1)
	backendProgressStopChanel := make(chan struct{})
	h.backendProgressStop = backendProgressStopChanel
	go h.backend.NodeProgressLoop(backendProgressStopChanel, &h.loopsWg)

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

// isUseless checks if the peer is banned from discovery and ban it if it should be
func isUseless(node *enode.Node, name string) bool {
	return discfilter.Banned(node.ID(), node.Record())
}

// handle is the callback invoked to manage the life cycle of a peer.
func (h *handler) handle(p *peer) (err error) {
	p.Log().Trace("Connecting peer", "peer", p.ID(), "name", p.Name())

	useless := isUseless(p.Node(), p.Name())
	if !p.Peer.Info().Network.Trusted && useless && h.peers.UselessNum() >= h.maxPeers/10 {
		// don't allow more than 10% of useless peers
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

	if err := p.Handshake(h.NetworkID, myProgress, common.Hash(genesis)); err != nil {
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

func interfacesToEventIDs(ids []interface{}) hash.Events {
	res := make(hash.Events, len(ids))
	for i, id := range ids {
		res[i] = id.(hash.Event)
	}
	return res
}

func eventIDsToInterfaces(ids hash.Events) []interface{} {
	res := make([]interface{}, len(ids))
	for i, id := range ids {
		res[i] = id
	}
	return res
}

func interfacesToTxids(ids []interface{}) []common.Hash {
	res := make([]common.Hash, len(ids))
	for i, id := range ids {
		res[i] = id.(common.Hash)
	}
	return res
}

func txidsToInterfaces(ids []common.Hash) []interface{} {
	res := make([]interface{}, len(ids))
	for i, id := range ids {
		res[i] = id
	}
	return res
}

func (h *handler) handleTxHashes(p *peer, announces []common.Hash, receivedAt time.Time) {
	// Only request what we don't know
	toRequest := make([]common.Hash, 0, len(announces))

	// Mark the hashes as present at the remote node
	for _, id := range announces {
		// Mark transaction as seen for peer
		p.MarkTransaction(id)

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
	return
}

func (h *handler) handleTxs(p *peer, txs types.Transactions, receivedAt time.Time) {
	// Mark the hashes as present at the remote node
	for _, tx := range txs {

		id := tx.Hash()

		p.MarkTransaction(id)

		if !h.txHashCache.Contains(id) {
			h.txHashCache.Add(id, struct{}{}, 1)
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
	p.RequestEvents(toRequest)
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
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > protocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	defer caution.ExecuteAndReportError(&err, msg.Discard, "failed to discard message")

	// Read payload into buffer to avoid holding semaphore during network I/O (Slowloris protection)
	// We use a pooled buffer to minimize allocations.
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	if msg.Size > 0 {
		buf.Grow(int(msg.Size))
	}
	if _, err := buf.ReadFrom(msg.Payload); err != nil {
		return err
	}
	blob := buf.Bytes()

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
		if err := rlp.DecodeBytes(blob, &progress); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}

		if progress.Epoch < h.backend.GetEpoch() {
			p.Log().Warn("Dropping unsynced peer", "name", p.Name(), "ip", p.RemoteAddr(), "peer_epoch", progress.Epoch, "my_epoch", h.backend.GetEpoch())
			h.removePeer(p.id)
			return p2p.DiscUselessPeer
		}
		p.SetProgress(progress)

	case EvmTxsMsg:
		// Record timestamp before decoding
		recivedAt := time.Now()

		txs := txBufPool.Get().(types.Transactions)
		txs = txs[:0]
		defer txBufPool.Put(txs)
		if err := rlp.DecodeBytes(blob, &txs); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		if err := checkLenLimits(len(txs), txs); err != nil {
			return err
		}
		h.handleTxs(p, txs, recivedAt)

	case NewEvmTxHashesMsg:
		// Record timestamp before decoding
		recivedAt := time.Now()

		txHashes := txHashesBufPool.Get().([]common.Hash)
		txHashes = txHashes[:0]
		defer txHashesBufPool.Put(txHashes)
		if err := rlp.DecodeBytes(blob, &txHashes); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		if err := checkLenLimits(len(txHashes), txHashes); err != nil {
			return err
		}
		h.handleTxHashes(p, txHashes, recivedAt)

	case GetEvmTxsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil
		return nil

	case EventsMsg:
		// Record timestamp before decoding
		recivedAt := time.Now()

		events := eventBufPool.Get().(inter.EventPayloads)
		events = events[:0]
		defer eventBufPool.Put(events)
		if err := rlp.DecodeBytes(blob, &events); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		if err := checkLenLimits(len(events), events); err != nil {
			return err
		}
		h.handleEvents(p, events.Bases(), (events.Len() > 1), recivedAt)

	case NewEventIDsMsg:
		// Record timestamp before decoding
		recivedAt := time.Now()

		announces := eventIDsBufPool.Get().(hash.Events)
		announces = announces[:0]
		defer eventIDsBufPool.Put(announces)
		if err := rlp.DecodeBytes(blob, &announces); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		if err := checkLenLimits(len(announces), announces); err != nil {
			return err
		}
		h.handleEventHashes(p, announces, recivedAt)

	case GetEventsMsg:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case RequestEventsStream:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case EventsStreamResponse:
		// Sonicpeer: We dont want to waste bandwith so we skip this and just return nil

	case GetPeerInfosMsg:
		infos := []peerInfo{}
		peers := peerSlicePool.Get().([]*peer)
		peers = h.peers.ListTo(peers[:0])
		defer peerSlicePool.Put(peers)
		for _, peer := range peers {
			if peer.Useless() {
				continue
			}
			info := peer.endPoint.Load()
			if info == nil {
				continue
			}
			infos = append(infos, peerInfo{
				Enode: info.enode.String(),
			})
		}
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		if err := rlp.Encode(buf, peerInfoMsg{Peers: infos}); err != nil {
			return errResp(ErrDecode, "failed to encode peer infos: %v", err)
		}
		err := p2p.Send(p.rw, PeerInfosMsg, rlp.RawValue(buf.Bytes()))
		if err != nil {
			return err
		}

	case PeerInfosMsg:
		var infos peerInfoMsg
		if err := rlp.DecodeBytes(blob, &infos); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		reportedPeers := []*enode.Node{}
		for _, info := range infos.Peers {
			var enode enode.Node
			if err := enode.UnmarshalText([]byte(info.Enode)); err != nil {
				h.Log.Debug("Failed to unmarshal enode", "enode", info.Enode, "err", err)
			} else {
				reportedPeers = append(reportedPeers, &enode)
			}
		}
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
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		if err := rlp.Encode(buf, enode.String()); err != nil {
			return err
		}
		if err := p2p.Send(p.rw, EndPointUpdateMsg, rlp.RawValue(buf.Bytes())); err != nil {
			return err
		}

	case EndPointUpdateMsg:
		var encoded string
		if err := rlp.DecodeBytes(blob, &encoded); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}

		var enode enode.Node
		if err := enode.UnmarshalText([]byte(encoded)); err != nil {
			h.Log.Debug("Failed to unmarshal enode", "enode", encoded, "err", err)
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

func (h *handler) decideBroadcastAggressiveness(size int, passed time.Duration, peersNum int) int {
	// MEV: Always broadcast to everyone immediately to ensure maximum propagation speed.
	return peersNum
}

// BroadcastTxs will propagate a batch of transactions to all peers which are not known to
// already have the given transaction.
func (h *handler) BroadcastTxs(txs types.Transactions) {
	peers := peerSlicePool.Get().([]*peer)
	peers = h.peers.ListTo(peers[:0])
	defer peerSlicePool.Put(peers)
	if len(peers) == 0 {
		return
	}

	// Optimization: Iterate peers and filter txs for each peer using a reusable buffer.
	// This avoids allocating a map of slices and multiple slice resizes. Use pool to avoid heap alloc.
	packet := txBufPool.Get().(types.Transactions)
	if cap(packet) < len(txs) {
		packet = make(types.Transactions, 0, len(txs))
	}
	defer txBufPool.Put(packet)

	for _, p := range peers {
		packet = packet[:0] // Reset length, keep capacity
		packet = p.FilterKnownTxs(txs, packet)

		if len(packet) > 0 {
			SplitTransactions(packet, func(batch types.Transactions) {
				p.AsyncSendTransactions(batch, p.queue)
			})
		}
	}
}

// BroadcastTransaction broadcasts a transaction to all connected peers.
func (h *handler) BroadcastTx(tx *types.Transaction) {
	peers := peerSlicePool.Get().([]*peer)
	peers = h.peers.ListTo(peers[:0])
	defer peerSlicePool.Put(peers)

	txs := txBufPool.Get().(types.Transactions)
	txs = append(txs[:0], tx)
	defer txBufPool.Put(txs)

	for _, p := range peers {
		p.AsyncSendTransactions(txs, p.queue)
	}
}

// BroadcastEvents will propagate a batch of events to all peers which are not known to
// already have the given event.
func (h *handler) BroadcastEvents(events inter.EventPayloads) {
	peers := peerSlicePool.Get().([]*peer)
	peers = h.peers.ListTo(peers[:0])
	defer peerSlicePool.Put(peers)
	if len(peers) == 0 {
		return
	}

	// Optimization: Iterate peers and filter events for each peer using a reusable buffer.
	// This avoids allocating a map of slices and multiple slice resizes. Use pool to avoid heap alloc.
	packet := eventBufPool.Get().(inter.EventPayloads)
	if cap(packet) < len(events) {
		packet = make(inter.EventPayloads, 0, len(events))
	}
	defer eventBufPool.Put(packet)

	for _, p := range peers {
		packet = packet[:0] // Reset length, keep capacity
		packet = p.FilterKnownEvents(events, packet)

		if len(packet) > 0 {
			// Split events into smaller batches
			remaining := packet
			for len(remaining) > 0 {
				batchSize := 0
				var batch inter.EventPayloads
				for i, e := range remaining {
					batchSize += e.Size() + 1024
					batch = remaining[:i+1]
					if batchSize >= softResponseLimitSize || i+1 >= softLimitItems {
						break
					}
				}
				remaining = remaining[len(batch):]
				p.AsyncSendEvents(batch, p.queue)
			}
		}
	}
}

func (h *handler) peerInfoCollectionLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(h.config.Protocol.PeerInfoCollectionPeriod)
	defer ticker.Stop()
	defer h.loopsWg.Done()

	collect := func() {
		// Get a suggestion for a new peer.
		// The number of suggestions to fetch is a trade-off between discovery speed and network load.
		// The original value of 64 was highly aggressive and could lead to excessive connection attempts.
		// A smaller number like 8 reduces network chatter and connection churn, while still ensuring
		// a steady stream of new potential peers.
		suggested := make(map[string]struct{})
		for i := 0; i < 8; i++ {
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
		peers := peerSlicePool.Get().([]*peer)
		peers = h.peers.ListTo(peers[:0])
		// We can't defer Put here because of the loop structure and potential long execution,
		// but for simplicity in this loop it's safer to just let GC handle it or manually Put at end.
		// Given this is a ticker loop, let's manually Put.

		for _, peer := range peers {
			// If we do not have the peer's end-point or it is too old, request it.
			if info := peer.endPoint.Load(); info == nil || time.Since(info.timestamp) > h.config.Protocol.PeerEndPointUpdatePeriod {
				if err := peer.SendEndPointUpdateRequest(); err != nil {
					log.Warn("Failed to send end-point update request", "peer", peer.id, "err", err)
					// If the end-point update request fails, do not send the peer info request.
					continue
				}
			}

			if err := peer.SendPeerInfoRequest(); err != nil {
				log.Warn("Failed to send peer info request", "peer", peer.id, "err", err)
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
		peerSlicePool.Put(peers)
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

type persistedPeer struct {
	Enode string `json:"enode"`
}

func (h *handler) savePeers() {
	if h.dataDir == "" {
		return
	}

	path := filepath.Join(h.dataDir, "nodes.json")

	peers := peerSlicePool.Get().([]*peer)
	peers = h.peers.ListTo(peers[:0])
	defer peerSlicePool.Put(peers)

	nodes := make([]persistedPeer, 0, len(peers))
	for _, p := range peers {
		if n := p.Node(); n != nil {
			nodes = append(nodes, persistedPeer{
				Enode: n.String(),
			})
		}
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
		Network:     h.NetworkID,
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
