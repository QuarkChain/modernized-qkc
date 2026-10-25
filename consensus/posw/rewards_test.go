// Copyright 2026-2027 The quarkchain Authors
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

package posw

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestRegenesisRewards(t *testing.T) {
	for _, test := range []struct {
		number, offset uint64
		want           uint64
	}{
		{0, 19, 0}, {1, 18, 25}, {1, 19, 12}, {10, 19, 12}, {11, 19, 6}, {math.MaxUint64, math.MaxUint64, 0},
	} {
		config := &params.QuarkChainConfig{
			Reward: &params.RewardConfig{
				CoinbaseAmount: uint256.NewInt(100),
				EpochInterval:  10,
				DecayDivisor:   2,
				RewardTaxRate:  big.NewRat(1, 2),
			},
			OriginalBlockNumber: test.offset,
		}
		db, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			t.Fatal(err)
		}
		h := &types.Header{Number: new(big.Int).SetUint64(test.number), Coinbase: common.HexToAddress("0x01")}
		accumulateRewards(config, db, h)
		if got := db.GetBalance(h.Coinbase); !got.Eq(uint256.NewInt(test.want)) {
			t.Fatalf("height %d offset %d: %s want %d", test.number, test.offset, got, test.want)
		}
	}
}
