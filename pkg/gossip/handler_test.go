package gossip

import (
	"io"
	"testing"
	"time"

	"sonicpeer/pkg/gossip/topology"

	"github.com/Fantom-foundation/lachesis-base/inter/dag"
	"github.com/Fantom-foundation/lachesis-base/utils/cachescale"
	"github.com/Fantom-foundation/lachesis-base/utils/datasemaphore"
	"github.com/Fantom-foundation/lachesis-base/utils/wlru"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/0xsoniclabs/sonic/logger"
)

type mockLocalEndPointSource struct {
	node *enode.Node
}

func (m *mockLocalEndPointSource) GetLocalEndPoint() *enode.Node {
	return m.node
}

func setupTestHandler() *handler {
	// 1. Setup Handler dependencies
	// Use a smaller cache for the benchmark to ensure we hit it
	cache, err := wlru.New(1024*1024, 1024)
	if err != nil {
		panic(err)
	}

	eventCache, err := wlru.New(1024*1024, 1024)
	if err != nil {
		panic(err)
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

// TestHandleMsg_SlowRead_SemaphoreRelease verifies that the handler does not hold
// the global message semaphore while waiting for the message payload to be read.
// This prevents "Slow Read" DoS attacks where an attacker stalls the connection
// to exhaust the victim's semaphore slots.
func TestHandleMsg_SlowRead_SemaphoreRelease(t *testing.T) {
	h := setupTestHandler()

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
