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
		{0, 19, 0}, {1, 18, 25}, {1, 19, 12}, {10, 19, 12}, {11, 19, 6}, {71, 19, 0},
	} {
		config := &params.QuarkChainConfig{
			Reward: &params.RewardConfig{
				CoinbaseAmount: uint256.NewInt(100),
				EpochInterval:  10,
				DecayFactor:    big.NewRat(1, 2),
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

func TestRewardDecay(t *testing.T) {
	// Expected balances follow pyquarkchain: floor the epoch decay first,
	// then multiply by the local fee rate and floor again.
	for _, test := range []struct {
		name   string
		factor string
		amount uint64
		epoch  uint64
		want   uint64
	}{
		{"before decay", "22/25", 100, 0, 50},
		{"first epoch", "22/25", 100, 1, 44},
		{"second epoch", "22/25", 100, 2, 38},
		{"round once after exponentiation", "22/25", 100, 3, 34},
		{"slow decay", "22/25", 100, 6, 23},
		{"decay before tax", "22/25", 3, 1, 1},
		{"denominator limit", "99/100", 100, 100, 18},
		{"fully decayed", "22/25", 100, 100, 0},
		{"no decay", "1", 100, 100, 50},
		{"zero factor before decay", "0", 100, 0, 50},
		{"zero factor after decay", "0", 100, 1, 0},
		{"zero subsidy", "22/25", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			factor, ok := new(big.Rat).SetString(test.factor)
			if !ok {
				t.Fatal("invalid test factor")
			}
			config := &params.QuarkChainConfig{
				Reward: &params.RewardConfig{
					CoinbaseAmount: uint256.NewInt(test.amount),
					EpochInterval:  1,
					DecayFactor:    factor,
					RewardTaxRate:  big.NewRat(1, 2),
				},
				OriginalBlockNumber: test.epoch,
			}
			if err := config.Reward.Check(); err != nil {
				t.Fatal(err)
			}
			// A positive new block number with epoch zero is needed to exercise
			// the subsidy before its first decay without crediting genesis.
			if test.epoch == 0 {
				config.Reward.EpochInterval = 2
			} else {
				config.OriginalBlockNumber--
			}
			db, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
			if err != nil {
				t.Fatal(err)
			}
			h := &types.Header{Number: big.NewInt(1), Coinbase: common.HexToAddress("0x01")}
			accumulateRewards(config, db, h)
			if got := db.GetBalance(h.Coinbase); !got.Eq(uint256.NewInt(test.want)) {
				t.Fatalf("balance %s, want %d", got, test.want)
			}
			if factor.RatString() != test.factor {
				t.Fatalf("decay factor mutated: %s", factor)
			}
		})
	}
}
