package gossip

import (
	"context"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"
)

// TransactionBroadcaster defines the interface for broadcasting transactions.
type TransactionBroadcaster interface {
	BroadcastTxs(txs types.Transactions)
}

// PublicAPI provides an API to subscribe to new transactions.
type PublicAPI struct {
	feed *event.Feed
	p2p  TransactionBroadcaster
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
	txCh := make(chan *types.Transaction, 4096)
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
	txCh := make(chan *types.Transaction, 4096)
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
