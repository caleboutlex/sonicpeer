package gossip

import (
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

type syncStatus struct {
	maybeSynced uint32
}

func (ss *syncStatus) MaybeSynced() bool {
	return atomic.LoadUint32(&ss.maybeSynced) != 0
}

func (ss *syncStatus) MarkMaybeSynced() {
	atomic.StoreUint32(&ss.maybeSynced, uint32(1))
}

func (ss *syncStatus) AcceptEvents() bool {
	return true
}

func (ss *syncStatus) AcceptBlockRecords() bool {
	return false
}

func (ss *syncStatus) AcceptTxs() bool {
	return ss.MaybeSynced()
}

func (ss *syncStatus) RequestLLR() bool {
	return ss.MaybeSynced()
}

type txsync struct {
	p     *peer
	txids []common.Hash
}
