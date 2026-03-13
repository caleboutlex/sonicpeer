package gossip

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

// rpcBlock is a simplified struct to decode block information from eth_getBlockByNumber.
type block struct {
	Number  *hexutil.Big   `json:"number"`
	Epoch   hexutil.Uint64 `json:"epoch"`
	Atropos common.Hash    `json:"atropos"`
}

// BackendNodeClient defines the interface for communicating with a trusted, stateful backend node.
// This would typically be implemented with an RPC client.
type backend interface {
	GetChainID() (uint64, error)
	GetNodeProgress() (PeerProgress, error)
	SubscribeNewHeads(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error)
	ProgressUpdaterLoop(stop <-chan struct{})
}

// rpcBackendClient is an implementation of BackendNodeClient using JSON-RPC.
type backendClient struct {
	rpc      *rpc.Client
	progress atomic.Pointer[PeerProgress]
}

// newRpcBackendClient creates a new RPC client for the backend node.
func newBackendClient(ctx context.Context, url string) (*backendClient, error) {
	c, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	return &backendClient{rpc: c}, nil
}

func (c *backendClient) Init() error {
	prog, err := c.fetchNodeProgress()
	if err != nil {
		return err
	}

	log.Info("BackendClient: Initialized progress",
		"epoch", prog.Epoch,
		"lastBlock", prog.LastBlockIdx,
		"atropos", prog.LastBlockAtropos)

	c.progress.Store(prog)
	return nil
}

func (c *backendClient) NodeProgress() PeerProgress {
	p := c.progress.Load()
	if p == nil {
		return PeerProgress{}
	}
	return *p
}

func (c *backendClient) GetEpoch() idx.Epoch {
	p := c.progress.Load()
	if p == nil {
		return 0
	}
	return p.Epoch
}

// GetChainID fetches the chain ID from the backend node using the 'eth_chainId' RPC call.
func (c *backendClient) fetchChainID() (uint64, error) {
	var chainID hexutil.Uint64
	err := c.rpc.Call(&chainID, "eth_chainId")
	if err != nil {
		log.Error("Failed to fetch chain ID from backend", "err", err)
		return 0, err
	}
	return uint64(chainID), nil
}

// GetPeerProgress fetches the current consensus progress from the backend node
// using the standard 'eth_getBlockByNumber' RPC call.
func (c *backendClient) fetchNodeProgress() (*PeerProgress, error) {
	var head block
	err := c.rpc.Call(&head, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		log.Warn("Failed to fetch peer progress from backend", "err", err)
		return nil, err
	}
	if head.Number == nil {
		return nil, errors.New("latest block has no number")
	}
	if head.Epoch == 0 {
		return nil, errors.New("Backend returned block with Epoch 0. Ensure backend is a Sonic node.")
	}
	return &PeerProgress{
		Epoch:            idx.Epoch(head.Epoch),
		LastBlockIdx:     idx.Block(head.Number.ToInt().Uint64()),
		LastBlockAtropos: hash.Event(head.Atropos),
	}, nil
}

// SubscribeNewHeads subscribes to new block headers.
func (c *backendClient) SubscribeNewHeads(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error) {
	return c.rpc.Subscribe(ctx, "eth", ch, "newHeads")
}

func (c *backendClient) NodeProgressLoop(stop <-chan struct{}, wg *sync.WaitGroup) {
	// Subscribe to new heads
	heads := make(chan *types.Header)
	sub, err := c.SubscribeNewHeads(context.Background(), heads)
	if err != nil {
		log.Error("BackendClient: Failed to subscribe to new heads", "err", err)
		return
	}
	defer sub.Unsubscribe()
	defer wg.Done()
	log.Info("BackendClient: Subscribed to new heads")

	for {
		select {
		case <-heads:
			// Fetch the progress
			p, err := c.fetchNodeProgress()
			if err != nil {
				log.Error("BackendClient: Failed to fetch progress", "err", err)
				continue
			}
			c.progress.Store(p)

		case <-stop:
			return
		}
	}
}
