package gossip

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"testing"
	"time"

	"sonicpeer/pkg/gossip/topology"

	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/Fantom-foundation/lachesis-base/utils/datasemaphore"
	"github.com/Fantom-foundation/lachesis-base/utils/wlru"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/0xsoniclabs/sonic/logger"
)

// infiniteMsgReader implements p2p.MsgReadWriter for benchmarking.
// It returns the same message payload repeatedly, simulating a continuous stream of messages.
type infiniteMsgReader struct {
	code    uint64
	payload []byte
}

func (m *infiniteMsgReader) ReadMsg() (p2p.Msg, error) {
	// Return a new Msg with a fresh Reader for the payload, as ReadMsg consumes it.
	return p2p.Msg{
		Code:       m.code,
		Size:       uint32(len(m.payload)),
		Payload:    bytes.NewReader(m.payload),
		ReceivedAt: time.Now(),
	}, nil
}

func (m *infiniteMsgReader) WriteMsg(msg p2p.Msg) error {
	return nil
}

type mockLocalEndPointSource struct {
	node *enode.Node
}

func (m *mockLocalEndPointSource) GetLocalEndPoint() *enode.Node {
	return m.node
}

func setupTestHandler(b *testing.B) *handler {
	// 1. Setup Handler dependencies
	// Use a smaller cache for the benchmark to ensure we hit it
	cache, err := wlru.New(1024*1024, 1024)
	if err != nil {
		b.Fatal(err)
	}

	eventCache, err := wlru.New(1024*1024, 1024)
	if err != nil {
		b.Fatal(err)
	}

	// Mock backend with progress to avoid nil pointer dereferences if accessed
	backend := &backendClient{}
	backend.progress.Store(&PeerProgress{})

	return &handler{
		config: Config{
			Protocol: ProtocolConfig{
				// Set high limits to avoid blocking on semaphore during benchmark
				MsgsSemaphoreLimit:   dag.Metric{Num: 100000, Size: 100 * 1024 * 1024},
				MsgsSemaphoreTimeout: time.Second,
			},
		},
		msgSemaphore:      datasemaphore.New(dag.Metric{Num: 100000, Size: 100 * 1024 * 1024}, nil),
		peers:             newPeerSet(),
		txHashCache:       cache,
		eventIDCache:      eventCache,
		backend:           backend,
		connectionAdvisor: topology.NewConnectionAdvisor(enode.ID{}),
		localEndPointSource: &mockLocalEndPointSource{
			node: enode.MustParse("enode://79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8@127.0.0.1:30303"),
		},
		Instance: logger.New("TEST"),
	}
}

// BenchmarkHandleMsg_EvmTxs-8      1000000              1146 ns/op             640 B/op         18 allocs/op
func BenchmarkHandleMsg_EvmTxs(b *testing.B) {
	h := setupTestHandler(b)

	// Create a batch of transactions
	count := 1
	txs := make(types.Transactions, count)
	key, _ := crypto.GenerateKey()
	signer := types.HomesteadSigner{}

	for i := 0; i < count; i++ {
		tx, err := types.SignTx(types.NewTransaction(uint64(i), common.Address{}, big.NewInt(100), 21000, big.NewInt(1), nil), signer, key)
		if err != nil {
			b.Fatal(err)
		}
		txs[i] = tx
	}

	// Encode transactions to RLP for the message payload
	payload, err := rlp.EncodeToBytes(txs)
	if err != nil {
		b.Fatal(err)
	}

	// Setup Peer with infinite reader
	rw := &infiniteMsgReader{code: EvmTxsMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_NewEvmTxHashes-8      2405580               642.7 ns/op           176 B/op          6 allocs/op
func BenchmarkHandleMsg_NewEvmTxHashes(b *testing.B) {
	h := setupTestHandler(b)

	count := 1
	hashes := make([]common.Hash, count)
	for i := 0; i < count; i++ {
		hashes[i] = common.BytesToHash(crypto.Keccak256([]byte{byte(i)}))
	}

	payload, err := rlp.EncodeToBytes(hashes)
	if err != nil {
		b.Fatal(err)
	}

	rw := &infiniteMsgReader{code: NewEvmTxHashesMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_NewEventIDs-8          184945              6958 ns/op            3344 B/op          6 allocs/op
func BenchmarkHandleMsg_NewEventIDs(b *testing.B) {
	h := setupTestHandler(b)

	count := 100
	ids := make(hash.Events, count)
	for i := 0; i < count; i++ {
		ids[i] = hash.BytesToEvent(crypto.Keccak256([]byte{byte(i)}))
	}

	payload, err := rlp.EncodeToBytes(ids)
	if err != nil {
		b.Fatal(err)
	}

	rw := &infiniteMsgReader{code: NewEventIDsMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_GetPeerInfos-8        2243564               585.5 ns/op           272 B/op          8 allocs/op
func BenchmarkHandleMsg_GetPeerInfos(b *testing.B) {
	h := setupTestHandler(b)

	// Payload is empty list for GetPeerInfosMsg
	payload, _ := rlp.EncodeToBytes([]interface{}{})

	rw := &infiniteMsgReader{code: GetPeerInfosMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_PeerInfos-8             55868             21742 ns/op            5217 B/op         95 allocs/op
func BenchmarkHandleMsg_PeerInfos(b *testing.B) {
	h := setupTestHandler(b)

	count := 1
	key, _ := crypto.GenerateKey()
	pub := crypto.FromECDSAPub(&key.PublicKey)
	infos := make([]peerInfo, count)
	for i := 0; i < count; i++ {
		infos[i] = peerInfo{Enode: fmt.Sprintf("enode://%x@127.0.0.1:30303", pub[1:])}
	}
	msg := peerInfoMsg{Peers: infos}

	payload, err := rlp.EncodeToBytes(msg)
	if err != nil {
		b.Fatal(err)
	}

	rw := &infiniteMsgReader{code: PeerInfosMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_GetEndPoint-8          221518              5472 ns/op            1464 B/op         31 allocs/op
func BenchmarkHandleMsg_GetEndPoint(b *testing.B) {
	h := setupTestHandler(b)

	payload, _ := rlp.EncodeToBytes([]interface{}{})

	rw := &infiniteMsgReader{code: GetEndPointMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandleMsg_EndPointUpdate-8        56359             21389 ns/op            5147 B/op         92 allocs/op
func BenchmarkHandleMsg_EndPointUpdate(b *testing.B) {
	h := setupTestHandler(b)

	enodeStr := "enode://79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8@127.0.0.1:30303"
	payload, err := rlp.EncodeToBytes(enodeStr)
	if err != nil {
		b.Fatal(err)
	}

	rw := &infiniteMsgReader{code: EndPointUpdateMsg, payload: payload}
	p2pPeer := p2p.NewPeer(enode.ID{}, "bench-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if err := h.handleMsg(p); err != nil {
			b.Fatal(err)
		}
	}
}

// TestHandleMsg_SlowRead_SemaphoreRelease verifies that the handler does not hold
// the global message semaphore while waiting for the message payload to be read.
// This prevents "Slow Read" DoS attacks where an attacker stalls the connection
// to exhaust the victim's semaphore slots.
func TestHandleMsg_SlowRead_SemaphoreRelease(t *testing.T) {
	h := setupTestHandler(&testing.B{}) // Reuse setup helper

	// 1. Configure a very restrictive semaphore (capacity for only 1 message)
	// If the handler holds the semaphore during the read, we won't be able to acquire it.
	msgSize := uint64(100)
	h.msgSemaphore = datasemaphore.New(dag.Metric{Num: 1, Size: msgSize}, nil)

	// 2. Create a "Slow Peer" that delays the payload delivery by 200ms
	delay := 200 * time.Millisecond
	rw := &slowMsgReader{
		code:    EvmTxsMsg,
		size:    uint32(msgSize),
		payload: make([]byte, msgSize), // Dummy payload
		delay:   delay,
	}
	p2pPeer := p2p.NewPeer(enode.ID{}, "slow-peer", nil)
	p := newPeer(65, p2pPeer, rw, DefaultPeerCacheConfig(cachescale.Identity))

	// 3. Start handling the message in a separate goroutine
	errCh := make(chan error)
	go func() {
		errCh <- h.handleMsg(p)
	}()

	// 4. Wait a bit to ensure handleMsg has started and is blocked in the "Slow Read"
	time.Sleep(delay / 2)

	// 5. Attempt to acquire the semaphore from the main thread.
	// - If the handler IS holding the semaphore (VULNERABLE), this will fail/block.
	// - If the handler IS NOT holding the semaphore (FIXED), this will succeed.
	est := dag.Metric{Num: 1, Size: 1}
	if !h.msgSemaphore.TryAcquire(est) {
		t.Fatal("VULNERABILITY DETECTED: Semaphore is held during network read! The node is susceptible to Slow Read DoS.")
	}
	h.msgSemaphore.Release(est)

	// Cleanup
	<-errCh
}

// slowMsgReader simulates a connection that delivers the message payload very slowly.
type slowMsgReader struct {
	code    uint64
	size    uint32
	payload []byte
	delay   time.Duration
}

func (m *slowMsgReader) ReadMsg() (p2p.Msg, error) {
	return p2p.Msg{
		Code:       m.code,
		Size:       m.size,
		Payload:    &slowReader{data: m.payload, delay: m.delay},
		ReceivedAt: time.Now(),
	}, nil
}

func (m *slowMsgReader) WriteMsg(msg p2p.Msg) error {
	return nil
}

type slowReader struct {
	data  []byte
	delay time.Duration
	read  bool
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	if !r.read {
		time.Sleep(r.delay)
		r.read = true
	}
	n = copy(p, r.data)
	r.data = r.data[n:]
	return n, io.EOF
}
