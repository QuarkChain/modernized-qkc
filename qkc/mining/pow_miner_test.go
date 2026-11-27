// Copyright 2026 The quarkchain Authors
// This file is part of the quarkchain library.
//
// The quarkchain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The quarkchain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the quarkchain library. If not, see <http://www.gnu.org/licenses/>.

package mining

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
)

type countingWorkBackend struct {
	MinerBackend
	builds byte
}

func (*countingWorkBackend) MiningHead() common.Hash {
	return common.Hash{42}
}

func (*countingWorkBackend) MiningAnchor() common.Hash {
	return common.Hash{}
}

func (b *countingWorkBackend) BuildMiningWork(context.Context, common.Address) (*Candidate, error) {
	b.builds++
	return &Candidate{
		MiningWork: MiningWork{SealHash: common.Hash{b.builds}, Difficulty: big.NewInt(1)},
		Parent:     b.MiningHead(),
	}, nil
}

func TestGetWorkRebuildsEvictedCandidate(t *testing.T) {
	m := NewPoWMiner(new(countingWorkBackend), PoWMinerConfig{WorkRefresh: time.Hour})
	// Both coinbases stay in current, but only one candidate fits in works.
	m.works = lru.NewBasicLRU[common.Hash, *Candidate](1)
	a, b := common.Address{1}, common.Address{2}
	for _, step := range []struct {
		coinbase common.Address
		wantHash common.Hash
	}{
		{a, common.Hash{1}},
		{a, common.Hash{1}},
		{b, common.Hash{2}},
		{a, common.Hash{3}},
	} {
		work, err := m.GetWork(t.Context(), step.coinbase)
		if err != nil {
			t.Fatal(err)
		}
		if work.SealHash != step.wantHash {
			t.Fatalf("work for %s: got %s, want %s", step.coinbase, work.SealHash, step.wantHash)
		}
	}
}
