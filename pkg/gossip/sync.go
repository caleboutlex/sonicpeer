// Copyright 2025 Sonic Operations Ltd
// This file is part of the Sonic Client
//
// Sonic is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Sonic is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Sonic. If not, see <http://www.gnu.org/licenses/>.

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
