package gossip

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/Fantom-foundation/lachesis-base/utils/datasemaphore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/0xsoniclabs/sonic/gossip/protocols/dag/dagstream"
	"github.com/0xsoniclabs/sonic/inter"
)

var (
	errNotRegistered = errors.New("peer is not registered")
)

var (
	sentTxsPromotedCounter    = metrics.GetOrRegisterCounter("p2p_sent_txs_promoted", nil)
	droppedTxsPromotedCounter = metrics.GetOrRegisterCounter("p2p_dropped_txs_promoted", nil)
	sentTxsRequestedCounter   = metrics.GetOrRegisterCounter("p2p_sent_txs_requested", nil)
	sentTxHashesCounter       = metrics.GetOrRegisterCounter("p2p_sent_tx_hashes", nil)
)

const (
	handshakeTimeout = 15 * time.Second
)

// PeerInfo represents a short summary of the sub-protocol metadata known
// about a connected peer.
type PeerInfo struct {
	Version     uint      `json:"version"` // protocol version negotiated
	Epoch       idx.Epoch `json:"epoch"`
	NumOfBlocks idx.Block `json:"blocks"`
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

type broadcastItem struct {
	Code uint64
	Raw  rlp.RawValue
	buf  *bytes.Buffer
}

type peer struct {
	*p2p.Peer
	id                  string
	cfg                 PeerCacheConfig
	rw                  p2p.MsgReadWriter
	version             uint               // Protocol version negotiated
	knownTxs            *txCache           // Set of transaction hashes known to be known by this peer
	knownEvents         *eventCache        // Set of event hashes known to be known by this peer
	queue               chan broadcastItem // queue of items to send
	queuedDataSemaphore *datasemaphore.DataSemaphore
	term                chan struct{} // Termination channel to stop the broadcaster
	progress            PeerProgress
	useless             uint32
	endPoint            atomic.Pointer[peerEndPointInfo]
	sync.RWMutex
}

type peerEndPointInfo struct {
	enode     enode.Node
	timestamp time.Time
}

func (p *peer) Useless() bool {
	return atomic.LoadUint32(&p.useless) != 0
}

func (p *peer) SetUseless() {
	atomic.StoreUint32(&p.useless, 1)
}

func (p *peer) SetProgress(x PeerProgress) {
	p.Lock()
	defer p.Unlock()

	p.progress = x
}

func (p *peer) GetProgress() PeerProgress {
	p.RLock()
	defer p.RUnlock()

	return p.progress
}

func (p *peer) InterestedIn(h hash.Event) bool {
	e := h.Epoch()

	p.RLock()
	defer p.RUnlock()

	return e != 0 &&
		p.progress.Epoch != 0 &&
		(e == p.progress.Epoch || e == p.progress.Epoch+1) &&
		!p.knownEvents.Contains(h)
}

func (a *PeerProgress) Less(b PeerProgress) bool {
	if a.Epoch != b.Epoch {
		return a.Epoch < b.Epoch
	}
	return a.LastBlockIdx < b.LastBlockIdx
}

func newPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter, cfg PeerCacheConfig) *peer {
	peer := &peer{
		cfg:                 cfg,
		Peer:                p,
		rw:                  rw,
		version:             version,
		id:                  p.ID().String(),
		knownTxs:            newTxCache(cfg.MaxKnownTxs),
		knownEvents:         newEventCache(cfg.MaxKnownEvents),
		queue:               make(chan broadcastItem, cfg.MaxQueuedItems),
		queuedDataSemaphore: datasemaphore.New(dag.Metric{Num: cfg.MaxQueuedItems, Size: cfg.MaxQueuedSize}, getSemaphoreWarningFn("Peers queue")),
		term:                make(chan struct{}),
	}

	go peer.broadcast(peer.queue)

	return peer
}

func (p *peer) Send(msgCode uint64, data rlp.RawValue) error {
	return p2p.Send(p.rw, msgCode, data)
}

// broadcast is a write loop that multiplexes event propagations, announcements
// and transaction broadcasts into the remote peer. The goal is to have an async
// writer that does not lock up node internals.
func (p *peer) broadcast(queue chan broadcastItem) {
	for {
		select {
		case item := <-queue:
			_ = p2p.Send(p.rw, item.Code, item.Raw)
			p.queuedDataSemaphore.Release(memSize(item.Raw))
			if item.buf != nil {
				item.buf.Reset()
				bufPool.Put(item.buf)
			}

		case <-p.term:
			return
		}
	}
}

// Close signals the broadcast goroutine to terminate.
func (p *peer) Close() {
	p.queuedDataSemaphore.Terminate()
	close(p.term)
}

// CollectMetadata fetches the geo location of the peer and other interesting metadata.
func (p *peer) CollectMetadata() {
	// This functionality has been removed to improve performance.
}

func (p *peer) ConfirmEndPointUpdate() {
	// This was used for latency measurement, which has been removed.
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *peer) Info() *PeerInfo {
	p.RLock()
	defer p.RUnlock()

	return &PeerInfo{
		Version:     p.version,
		Epoch:       p.progress.Epoch,
		NumOfBlocks: p.progress.LastBlockIdx,
	}
}

// MarkEvent marks a event as known for the peer, ensuring that the event will
// never be propagated to this particular peer.
func (p *peer) MarkEvent(hash hash.Event) {
	p.knownEvents.Add(hash)
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
func (p *peer) MarkTransaction(hash common.Hash) {
	p.knownTxs.Add(hash)
}

// SendTransactionHashes sends transaction hashes to the peer and includes the hashes
// in its transaction hash set for future reference.
func (p *peer) SendTransactionHashes(txids []common.Hash) error {
	// Mark all the transactions as known, but ensure we don't overflow our limits
	for _, txid := range txids {
		p.knownTxs.Add(txid)
	}
	sentTxHashesCounter.Inc(int64(len(txids)))

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	// Each hash is 32 bytes + 1 byte RLP prefix.
	size := len(txids)*33 + 100
	buf.Grow(size)

	if err := rlp.Encode(buf, txids); err != nil {
		return err
	}
	return p2p.Send(p.rw, NewEvmTxHashesMsg, rlp.RawValue(buf.Bytes()))
}

func memSize(v rlp.RawValue) dag.Metric {
	return dag.Metric{Num: 1, Size: uint64(len(v) + 1024)}
}

func (p *peer) asyncSendEncodedItem(raw rlp.RawValue, code uint64, queue chan broadcastItem, buf *bytes.Buffer) bool {
	if !p.queuedDataSemaphore.TryAcquire(memSize(raw)) {
		if buf != nil {
			bufPool.Put(buf)
		}
		return false
	}
	item := broadcastItem{
		Code: code,
		Raw:  raw,
		buf:  buf,
	}
	select {
	case queue <- item:
		return true
	case <-p.term:
	default:
	}
	p.queuedDataSemaphore.Release(memSize(raw))
	if buf != nil {
		bufPool.Put(buf)
	}
	return false
}

func (p *peer) enqueueSendEncodedItem(raw rlp.RawValue, code uint64, queue chan broadcastItem, buf *bytes.Buffer) {
	if !p.queuedDataSemaphore.Acquire(memSize(raw), 10*time.Second) {
		if buf != nil {
			bufPool.Put(buf)
		}
		return
	}
	item := broadcastItem{
		Code: code,
		Raw:  raw,
		buf:  buf,
	}
	select {
	case queue <- item:
		return
	case <-p.term:
	}
	p.queuedDataSemaphore.Release(memSize(raw))
	if buf != nil {
		bufPool.Put(buf)
	}
}

func SplitTransactions(txs types.Transactions, fn func(types.Transactions)) {
	// divide big batch into smaller ones
	for len(txs) > 0 {
		batchSize := 0
		var batch types.Transactions
		for i, tx := range txs {
			batchSize += int(tx.Size()) + 1024
			batch = txs[:i+1]
			if batchSize >= softResponseLimitSize || i+1 >= softLimitItems {
				break
			}
		}
		txs = txs[len(batch):]
		fn(batch)
	}
}

// SplitEvents is a helper function to divide a large event slice into
// smaller chunks to avoid exceeding message size limits.
func SplitEvents(events dag.Events, fn func(dag.Events)) {
	// divide big batch into smaller ones
	for len(events) > 0 {
		batchSize := 0
		var batch dag.Events
		for i, e := range events {
			batchSize += e.Size() + 1024
			batch = events[:i+1]
			if batchSize >= softResponseLimitSize || i+1 >= softLimitItems {
				break
			}
		}
		events = events[len(batch):]
		fn(batch)
	}
}

// AsyncSendTransactions queues list of transactions propagation to a remote
// peer. If the peer's broadcast queue is full, the transactions are silently dropped.
func (p *peer) AsyncSendTransactions(txs types.Transactions, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	size := 0
	for _, tx := range txs {
		size += int(tx.Size()) + 100 // RLP overhead estimate
	}
	buf.Grow(size)

	if err := rlp.Encode(buf, txs); err != nil {
		bufPool.Put(buf)
		return
	}

	if p.asyncSendEncodedItem(rlp.RawValue(buf.Bytes()), EvmTxsMsg, queue, buf) {
		sentTxsPromotedCounter.Inc(int64(len(txs)))
		// Mark all the transactions as known, but ensure we don't overflow our limits
		for _, tx := range txs {
			p.knownTxs.Add(tx.Hash())
		}
	} else {
		droppedTxsPromotedCounter.Inc(int64(len(txs)))
		p.Log().Debug("Dropping transactions propagation", "count", len(txs))
	}
}

// AsyncSendTransactionHashes queues list of transactions propagation to a remote
// peer. If the peer's broadcast queue is full, the transactions are silently dropped.
func (p *peer) AsyncSendTransactionHashes(txids []common.Hash, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	// Each hash is 32 bytes + 1 byte RLP prefix.
	size := len(txids)*33 + 100
	buf.Grow(size)

	if err := rlp.Encode(buf, txids); err != nil {
		bufPool.Put(buf)
		return
	}

	if p.asyncSendEncodedItem(rlp.RawValue(buf.Bytes()), NewEvmTxHashesMsg, queue, buf) {
		sentTxHashesCounter.Inc(int64(len(txids)))
		// Mark all the transactions as known, but ensure we don't overflow our limits
		for _, tx := range txids {
			p.knownTxs.Add(tx)
		}
	} else {
		p.Log().Debug("Dropping tx announcement", "count", len(txids))
	}
}

// EnqueueSendTransactions queues list of transactions propagation to a remote
// peer.
// The method is blocking in a case if the peer's broadcast queue is full.
func (p *peer) EnqueueSendTransactions(txs types.Transactions, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	size := 0
	for _, tx := range txs {
		size += int(tx.Size()) + 100
	}
	buf.Grow(size)
	if err := rlp.Encode(buf, txs); err != nil {
		bufPool.Put(buf)
		return
	}
	p.enqueueSendEncodedItem(rlp.RawValue(buf.Bytes()), EvmTxsMsg, queue, buf)
	sentTxsRequestedCounter.Inc(int64(len(txs)))
	// Mark all the transactions as known, but ensure we don't overflow our limits
	for _, tx := range txs {
		p.knownTxs.Add(tx.Hash())
	}
}

// SendEventIDs announces the availability of a number of events through
// a hash notification.
func (p *peer) SendEventIDs(hashes []hash.Event) error {
	// Mark all the event hashes as known, but ensure we don't overflow our limits
	for _, hash := range hashes {
		p.knownEvents.Add(hash)
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	// Each hash is 32 bytes + 1 byte RLP prefix.
	size := len(hashes)*33 + 100
	buf.Grow(size)

	if err := rlp.Encode(buf, hashes); err != nil {
		return err
	}
	return p2p.Send(p.rw, NewEventIDsMsg, rlp.RawValue(buf.Bytes()))
}

// AsyncSendEventIDs queues the availability of a event for propagation to a
// remote peer. If the peer's broadcast queue is full, the event is silently
// dropped.
func (p *peer) AsyncSendEventIDs(ids hash.Events, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	// Each hash is 32 bytes + 1 byte RLP prefix.
	size := len(ids)*33 + 100
	buf.Grow(size)

	if err := rlp.Encode(buf, ids); err != nil {
		bufPool.Put(buf)
		return
	}

	if p.asyncSendEncodedItem(rlp.RawValue(buf.Bytes()), NewEventIDsMsg, queue, buf) {
		// Mark all the event hash as known, but ensure we don't overflow our limits
		for _, id := range ids {
			p.knownEvents.Add(id)
		}
	} else {
		p.Log().Debug("Dropping event announcement", "count", len(ids))
	}
}

// SendEvents propagates a batch of events to a remote peer.
func (p *peer) SendEvents(events inter.EventPayloads) error {
	// Mark all the event hash as known, but ensure we don't overflow our limits
	for _, event := range events {
		p.knownEvents.Add(event.ID())
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	size := 0
	for _, e := range events {
		size += e.Size() + 100 // RLP overhead estimate
	}
	buf.Grow(size)

	if err := rlp.Encode(buf, events); err != nil {
		return err
	}
	return p2p.Send(p.rw, EventsMsg, rlp.RawValue(buf.Bytes()))
}

// SendEventsRLP propagates a batch of RLP events to a remote peer.
func (p *peer) SendEventsRLP(events []rlp.RawValue, ids []hash.Event) error {
	// Mark all the event hash as known, but ensure we don't overflow our limits
	for _, id := range ids {
		p.knownEvents.Add(id)
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()

	size := 0
	for _, e := range events {
		size += len(e)
	}
	buf.Grow(size + 100)

	if err := rlp.Encode(buf, events); err != nil {
		return err
	}
	return p2p.Send(p.rw, EventsMsg, rlp.RawValue(buf.Bytes()))
}

// AsyncSendEvents queues an entire event for propagation to a remote peer.
// If the peer's broadcast queue is full, the events are silently dropped.
func (p *peer) AsyncSendEvents(events inter.EventPayloads, queue chan broadcastItem) bool {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	// Pre-allocate buffer to avoid resizes
	size := 0
	for _, e := range events {
		size += e.Size() + 100 // RLP overhead estimate
	}
	buf.Grow(size)

	if err := rlp.Encode(buf, events); err != nil {
		bufPool.Put(buf)
		return false
	}

	if p.asyncSendEncodedItem(rlp.RawValue(buf.Bytes()), EventsMsg, queue, buf) {
		// Mark all the event hash as known, but ensure we don't overflow our limits
		for _, event := range events {
			p.knownEvents.Add(event.ID())
		}
		return true
	}
	p.Log().Debug("Dropping event propagation", "count", len(events))
	return false
}

// EnqueueSendEventsRLP queues an entire RLP event for propagation to a remote peer.
// The method is blocking in a case if the peer's broadcast queue is full.
func (p *peer) EnqueueSendEventsRLP(events []rlp.RawValue, ids []hash.Event, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()

	size := 0
	for _, e := range events {
		size += len(e)
	}
	buf.Grow(size + 100)
	if err := rlp.Encode(buf, events); err != nil {
		bufPool.Put(buf)
		return
	}
	p.enqueueSendEncodedItem(rlp.RawValue(buf.Bytes()), EventsMsg, queue, buf)
	// Mark all the event hash as known, but ensure we don't overflow our limits
	for _, id := range ids {
		p.knownEvents.Add(id)
	}
}

// AsyncSendProgress queues a progress propagation to a remote peer.
// If the peer's broadcast queue is full, the progress is silently dropped.
func (p *peer) AsyncSendProgress(progress PeerProgress, queue chan broadcastItem) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Grow(256)
	if err := rlp.Encode(buf, progress); err != nil {
		bufPool.Put(buf)
		return
	}
	if !p.asyncSendEncodedItem(rlp.RawValue(buf.Bytes()), ProgressMsg, queue, buf) {
		p.Log().Debug("Dropping peer progress propagation")
	}
}

func (p *peer) RequestEvents(ids hash.Events) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)

	// divide big batch into smaller ones
	for start := 0; start < len(ids); start += softLimitItems {
		end := len(ids)
		if end > start+softLimitItems {
			end = start + softLimitItems
		}
		batch := ids[start:end]
		p.Log().Debug("Fetching batch of events", "count", len(batch))

		buf.Reset()
		// Pre-allocate buffer to avoid resizes
		// Each hash is 32 bytes + 1 byte RLP prefix.
		size := len(batch)*33 + 100
		buf.Grow(size)

		if err := rlp.Encode(buf, batch); err != nil {
			return err
		}
		if err := p2p.Send(p.rw, GetEventsMsg, rlp.RawValue(buf.Bytes())); err != nil {
			return err
		}
	}
	return nil
}

func (p *peer) RequestTransactions(txids []common.Hash) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)

	// divide big batch into smaller ones
	for start := 0; start < len(txids); start += softLimitItems {
		end := len(txids)
		if end > start+softLimitItems {
			end = start + softLimitItems
		}
		batch := txids[start:end]
		p.Log().Debug("Fetching batch of transactions", "count", len(batch))

		buf.Reset()
		// Pre-allocate buffer to avoid resizes
		// Each hash is 32 bytes + 1 byte RLP prefix.
		size := len(batch)*33 + 100
		buf.Grow(size)

		if err := rlp.Encode(buf, batch); err != nil {
			return err
		}
		if err := p2p.Send(p.rw, GetEvmTxsMsg, rlp.RawValue(buf.Bytes())); err != nil {
			return err
		}
	}
	return nil
}

func (p *peer) SendEventsStream(r dagstream.Response, ids hash.Events) error {
	// Mark all the event hash as known, but ensure we don't overflow our limits
	for _, id := range ids {
		p.knownEvents.Add(id)
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	buf.Grow(4096) // Heuristic for stream response size

	if err := rlp.Encode(buf, r); err != nil {
		return err
	}
	return p2p.Send(p.rw, EventsStreamResponse, rlp.RawValue(buf.Bytes()))
}

func (p *peer) RequestEventsStream(r dagstream.Request) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	buf.Grow(512) // Heuristic for request size

	if err := rlp.Encode(buf, r); err != nil {
		return err
	}
	return p2p.Send(p.rw, RequestEventsStream, rlp.RawValue(buf.Bytes()))
}

// Handshake executes the protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis object.
func (p *peer) Handshake(network uint64, progress PeerProgress, genesis common.Hash) error {
	// Send out own handshake in a new thread
	errc := make(chan error, 2)
	var handshake handshakeData // safe to read after two values have been received from errc

	go func() {
		// send both HandshakeMsg and ProgressMsg
		buf := bufPool.Get().(*bytes.Buffer)
		defer bufPool.Put(buf)
		buf.Reset()
		buf.Grow(512)

		if err := rlp.Encode(buf, &handshakeData{
			ProtocolVersion: uint32(p.version),
			NetworkID:       network,
			Genesis:         genesis,
		}); err != nil {
			errc <- err
			return
		}
		err := p2p.Send(p.rw, HandshakeMsg, rlp.RawValue(buf.Bytes()))
		if err != nil {
			errc <- err
		}
		errc <- p.SendProgress(progress)
	}()
	go func() {
		errc <- p.readStatus(network, &handshake, genesis)
		// do not expect ProgressMsg here, because eth62 clients won't send it
	}()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return p2p.DiscReadTimeout
		}
	}
	return nil
}

func (p *peer) SendProgress(progress PeerProgress) error {
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	buf.Grow(256) // Progress message is small

	if err := rlp.Encode(buf, progress); err != nil {
		return err
	}
	return p2p.Send(p.rw, ProgressMsg, rlp.RawValue(buf.Bytes()))
}

func (p *peer) readStatus(network uint64, handshake *handshakeData, genesis common.Hash) (err error) {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != HandshakeMsg {
		return errResp(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, HandshakeMsg)
	}
	if msg.Size > protocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	// Decode the handshake and make sure everything matches
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	if msg.Size > 0 {
		buf.Grow(int(msg.Size))
	}
	if _, err := buf.ReadFrom(msg.Payload); err != nil {
		return errResp(ErrDecode, "msg %v: %v", msg, err)
	}
	if err := rlp.DecodeBytes(buf.Bytes(), handshake); err != nil {
		return errResp(ErrDecode, "msg %v: %v", msg, err)
	}

	// TODO: rm after all the nodes updated to #184
	if handshake.NetworkID == 0 {
		handshake.NetworkID = network
	}

	if handshake.Genesis != genesis {
		return errResp(ErrGenesisMismatch, "%x (!= %x)", handshake.Genesis[:8], genesis[:8])
	}
	if handshake.NetworkID != network {
		return errResp(ErrNetworkIDMismatch, "%d (!= %d)", handshake.NetworkID, network)
	}
	if uint(handshake.ProtocolVersion) != p.version {
		return errResp(ErrProtocolVersionMismatch, "%d (!= %d)", handshake.ProtocolVersion, p.version)
	}
	return nil
}

// SendPeerInfoRequest sends a request to the peer asking for an update of
// its list of peers.
func (p *peer) SendPeerInfoRequest() error {
	// If the peer doesn't support the peer info protocol, don't bother
	// sending the request. This request would lead to a disconnect
	// if the peer doesn't understand it.
	if !p.RunningCap(ProtocolName, []uint{_Sonic_64, _Sonic_65}) {
		return nil
	}
	return p2p.Send(p.rw, GetPeerInfosMsg, rlp.RawValue{0xc0})
}

// SendEndPointUpdateRequest sends a request to the peer asking for the peer's
// public enode address to be used to establish a connection to this peer.
func (p *peer) SendEndPointUpdateRequest() error {
	// If the peer doesn't support version 65 of this protocol, don't bother
	// sending the request. This request would lead to a disconnect
	// if the peer doesn't understand it.
	if !p.RunningCap(ProtocolName, []uint{_Sonic_65}) {
		return nil
	}
	return p2p.Send(p.rw, GetEndPointMsg, rlp.RawValue{0xc0})
}

// String implements fmt.Stringer.
func (p *peer) String() string {
	return fmt.Sprintf("Peer %s [%s]", p.id,
		fmt.Sprintf("sonic/%2d", p.version),
	)
}

type txCache struct {
	mu    sync.RWMutex
	items map[common.Hash]struct{}
	keys  []common.Hash
	next  int
	max   int
}

func newTxCache(max int) *txCache {
	return &txCache{
		items: make(map[common.Hash]struct{}, max),
		keys:  make([]common.Hash, 0, max),
		max:   max,
	}
}

func (c *txCache) Add(h common.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[h]; ok {
		return
	}
	if len(c.keys) < c.max {
		c.items[h] = struct{}{}
		c.keys = append(c.keys, h)
		return
	}

	// FIFO eviction
	delete(c.items, c.keys[c.next])
	c.keys[c.next] = h
	c.next = (c.next + 1) % c.max
	c.items[h] = struct{}{}
}

func (c *txCache) FilterNew(txs types.Transactions, dst types.Transactions) types.Transactions {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, tx := range txs {
		if _, ok := c.items[tx.Hash()]; !ok {
			dst = append(dst, tx)
		}
	}
	return dst
}

func (c *txCache) Contains(h common.Hash) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.items[h]
	return ok
}

func (c *txCache) Cardinality() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

func (p *peer) FilterKnownTxs(txs types.Transactions, dst types.Transactions) types.Transactions {
	return p.knownTxs.FilterNew(txs, dst)
}

func (p *peer) FilterKnownEvents(events inter.EventPayloads, dst inter.EventPayloads) inter.EventPayloads {
	return p.knownEvents.FilterNew(events, dst)
}

type eventCache struct {
	mu    sync.RWMutex
	items map[hash.Event]struct{}
	keys  []hash.Event
	next  int
	max   int
}

func newEventCache(max int) *eventCache {
	return &eventCache{
		items: make(map[hash.Event]struct{}, max),
		keys:  make([]hash.Event, 0, max),
		max:   max,
	}
}

func (c *eventCache) Add(h hash.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[h]; ok {
		return
	}
	if len(c.keys) < c.max {
		c.items[h] = struct{}{}
		c.keys = append(c.keys, h)
		return
	}

	// FIFO eviction
	delete(c.items, c.keys[c.next])
	c.keys[c.next] = h
	c.next = (c.next + 1) % c.max
	c.items[h] = struct{}{}
}

func (c *eventCache) FilterNew(events inter.EventPayloads, dst inter.EventPayloads) inter.EventPayloads {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range events {
		if _, ok := c.items[e.ID()]; !ok {
			dst = append(dst, e)
		}
	}
	return dst
}

func (c *eventCache) Contains(h hash.Event) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.items[h]
	return ok
}

func (c *eventCache) Cardinality() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
