package gossip

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
)

// TransactionBroadcaster defines the interface for broadcasting transactions.
type TransactionBroadcaster interface {
	BroadcastTxs(txs types.Transactions)
}

// PublicAPI provides an API to subscribe to new transactions.
type PublicAPI struct {
	feed *event.Feed
	p2p  TransactionBroadcaster // This is actually the handler, which also implements PeerCounter
}

// NewPublicAPI creates a new API.
func NewPublicAPI(feed *event.Feed, backend TransactionBroadcaster) *PublicAPI {
	return &PublicAPI{feed: feed, p2p: backend}
}

// NewPendingTransactions creates a subscription that sends new transaction hashes as they arrive.
// This is accessed via `eth_subscribe("newPendingTransactions")` over WS/IPC.
func (api *PublicAPI) NewPendingTransactions(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	// Create a channel for the feed updates
	txCh := make(chan *types.Transaction, 16384)
	sub := api.feed.Subscribe(txCh)

	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case tx := <-txCh:
				// Standard eth_subscribe("newPendingTransactions") returns hashes
				if err := notifier.Notify(rpcSub.ID, tx.Hash()); err != nil {
					return
				}
			case <-rpcSub.Err():
				return
			}
		}
	}()

	return rpcSub, nil
}

// NewFullPendingTransactions creates a subscription that sends full transaction objects as they arrive.
// This is accessed via `eth_subscribe("newFullPendingTransactions")` over WS/IPC.
func (api *PublicAPI) NewFullPendingTransactions(ctx context.Context) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return nil, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	// Create a channel for the feed updates
	txCh := make(chan *types.Transaction, 16384)
	sub := api.feed.Subscribe(txCh)

	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case tx := <-txCh:
				// Notify with the full transaction object instead of just the hash
				if err := notifier.Notify(rpcSub.ID, tx); err != nil {
					return
				}
			case <-rpcSub.Err():
				return
			}
		}
	}()

	return rpcSub, nil
}

// PublicNetAPI offers network related RPC methods
type PublicNetAPI struct {
	net            *p2p.Server
	networkVersion uint64
}

// NewPublicNetAPI creates a new net API instance.
func NewPublicNetAPI(net *p2p.Server, networkVersion uint64) *PublicNetAPI {
	return &PublicNetAPI{net, networkVersion}
}

// Listening returns an indication if the node is listening for network connections.
func (s *PublicNetAPI) Listening() bool {
	return true // always listening
}

// PeerCount returns the number of connected peers
func (s *PublicNetAPI) PeerCount() hexutil.Uint {
	return hexutil.Uint(s.net.PeerCount())
}

// Version returns the current ethereum protocol version.
func (s *PublicNetAPI) Version() string {
	return fmt.Sprintf("%d", s.networkVersion)
}
