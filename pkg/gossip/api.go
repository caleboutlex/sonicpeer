package gossip

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"
)

// TransactionBroadcaster defines the interface for broadcasting transactions.
type TransactionBroadcaster interface {
	BroadcastTxs(txs types.Transactions)
	BroadcastTx(tx *types.Transaction)
}

// PublicMevAPI provides an API to subscribe to new transactions.
type PublicAPI struct {
	feed    *event.Feed
	backend TransactionBroadcaster
}

// NewPublicMevAPI creates a new MEV API.
func NewPublicAPI(feed *event.Feed, backend TransactionBroadcaster) *PublicAPI {
	return &PublicAPI{feed: feed, backend: backend}
}

// SubscribePendingTransactions creates a subscription that sends new transactions as they arrive.
// This can be accessed via `mev_subscribePendingTransactions` over WS/IPC.
func (api *PublicAPI) SubscribePendingTransactions(ctx context.Context) (*rpc.Subscription, error) {
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

// SendTransaction broadcasts a transaction to the connected peers.
func (api *PublicAPI) SendTransaction(ctx context.Context, tx *types.Transaction) (common.Hash, error) {
	api.backend.BroadcastTx(tx)
	return tx.Hash(), nil
}
