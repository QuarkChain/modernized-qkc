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
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestDifficultyReference(t *testing.T) {
	config := &params.PoSWConfig{AdjustmentCutoff: 10, AdjustmentFactor: 2048, MinimumDifficulty: big.NewInt(1)}
	for _, test := range []struct{ delta, want uint64 }{{0, 2049000}, {9, 2049000}, {10, 2048000}, {20, 2047000}, {1000, 1949000}, {math.MaxUint64, 1949000}} {
		parent := big.NewInt(2048000)
		if got := CalcDifficulty(config, test.delta, 0, parent); !got.IsUint64() || got.Uint64() != test.want {
			t.Fatalf("delta %d: %v, want %d", test.delta, got, test.want)
		}
		if parent.Int64() != 2048000 {
			t.Fatal("modified parent")
		}
	}
	config.MinimumDifficulty.SetUint64(2000000)
	if got := CalcDifficulty(config, 1000, 0, big.NewInt(2048000)); got.Int64() != 2000000 {
		t.Fatal(got)
	} else {
		got.SetInt64(0)
		if config.MinimumDifficulty.Int64() != 2000000 {
			t.Fatal("result aliases configured minimum difficulty")
		}
	}
}

func TestAdjustedDifficulty(t *testing.T) {
	c := &params.PoSWConfig{StakingEnabled: true, WindowSize: 3, StakePerBlock: big.NewInt(100), DiffDivider: 100}
	base := big.NewInt(100)
	for _, test := range []struct {
		stakes *uint256.Int
		mined  uint64
		want   int64
	}{
		{uint256.NewInt(99), 0, 100}, {uint256.NewInt(100), 0, 1}, {uint256.NewInt(100), 1, 100},
		{uint256.NewInt(300), 2, 1}, {uint256.MustFromDecimal("1844674407370955161700"), 2, 1},
	} {
		if got := GetAdjustedDifficulty(c, big.NewInt(1), base, test.stakes, test.mined); got.Int64() != test.want {
			t.Fatalf("stakes %s, mined %d: %v", test.stakes, test.mined, got)
		}
	}
	if base.Int64() != 100 {
		t.Fatal("modified base difficulty")
	}
	c.StakingEnabled = false
	if got := GetAdjustedDifficulty(c, big.NewInt(1), base, uint256.NewInt(100), 0); got.Int64() != 100 {
		t.Fatal(got)
	}
}

func TestShardStakingDecay(t *testing.T) {
	for _, test := range []struct {
		name           string
		number, offset uint64
		stakes, mined  uint64
		want           int64
	}{
		{"genesis", 0, 30, 100, 0, 100},
		{"before epoch boundary", 1, 28, 68, 0, 100},
		{"at epoch boundary", 2, 28, 68, 0, 1},
		{"round once after exponentiation", 2, 28, 67, 0, 100},
		{"next epoch", 12, 28, 59, 0, 1},
		{"below next epoch stake", 12, 28, 58, 0, 100},
		{"mined quota exhausted", 2, 28, 68, 1, 100},
		{"zero decay falls back to configured stake", 1000, 0, 99, 0, 100},
		{"fallback still allows full stake", 1000, 0, 100, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := &params.PoSWConfig{
				StakingEnabled: true, WindowSize: 3, StakePerBlock: big.NewInt(100), DiffDivider: 100,
				PoSWStakingDecayEnabled: true,
				EpochInterval:           10, DecayFactor: big.NewRat(22, 25),
				OriginalBlockNumber: test.offset,
			}
			number, base := new(big.Int).SetUint64(test.number), big.NewInt(100)
			stakes := uint256.NewInt(test.stakes)
			if got := GetAdjustedDifficulty(config, number, base, stakes, test.mined); got.Int64() != test.want {
				t.Fatalf("shard difficulty %s, want %d", got, test.want)
			}
			// The root path uses the configured, fixed requirement at every epoch.
			rootConfig := *config
			rootConfig.PoSWStakingDecayEnabled = false
			if got := GetAdjustedDifficulty(&rootConfig, number, base, uint256.NewInt(99), 0); got.Int64() != 100 {
				t.Fatalf("root stake requirement decayed: difficulty %s", got)
			}
			if config.StakePerBlock.Uint64() != 100 || config.DecayFactor.Cmp(big.NewRat(22, 25)) != 0 ||
				number.Uint64() != test.number || base.Int64() != 100 || stakes.Uint64() != test.stakes {
				t.Fatal("staking decay modified its inputs")
			}
		})
	}
	// Disabled staking leaves the base difficulty unchanged.
	for _, config := range []*params.PoSWConfig{nil, {StakingEnabled: false}} {
		if got := GetAdjustedDifficulty(config, big.NewInt(1), big.NewInt(100), nil, 0); got.Int64() != 100 {
			t.Fatal(got)
		}
	}
}

// testHeaderChain is a linked header chain with the given coinbases, from
// genesis up. Missing headers are not found.
type testHeaderChain struct {
	headers []*types.Header
	missing map[uint64]bool
}

func newTestHeaderChain(coinbases ...common.Address) *testHeaderChain {
	c := &testHeaderChain{missing: make(map[uint64]bool)}
	for i, coinbase := range coinbases {
		header := &types.Header{Number: big.NewInt(int64(i)), Coinbase: coinbase}
		if i > 0 {
			header.ParentHash = c.headers[i-1].Hash()
		}
		c.headers = append(c.headers, header)
	}
	return c
}

func (c *testHeaderChain) Config() *params.ChainConfig { return nil }

func (c *testHeaderChain) CurrentHeader() *types.Header { return c.headers[len(c.headers)-1] }

func (c *testHeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if header := c.GetHeaderByNumber(number); header != nil && header.Hash() == hash {
		return header
	}
	return nil
}

func (c *testHeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	if number >= uint64(len(c.headers)) || c.missing[number] {
		return nil
	}
	return c.headers[number]
}

func (c *testHeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	for _, header := range c.headers {
		if header.Hash() == hash {
			return c.GetHeaderByNumber(header.Number.Uint64())
		}
	}
	return nil
}

// candidate returns a block by coinbase on top of the chain, with base difficulty 100.
func (c *testHeaderChain) candidate(coinbase common.Address) *types.Header {
	parent := c.CurrentHeader()
	return &types.Header{
		ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number, common.Big1),
		Coinbase: coinbase, MixDigest: common.BigToHash(big.NewInt(100)),
	}
}

// testStakes reports fixed locked stakes and records the state they are read at.
type testStakes struct {
	stakes uint64
	err    error
	at     *types.Header
}

func (s *testStakes) GetShardLockedStakes(header *types.Header, staker common.Address) (*uint256.Int, error) {
	s.at = header
	return uint256.NewInt(s.stakes), s.err
}

func TestAdjustedShardDifficulty(t *testing.T) {
	a, b := common.Address{0xaa}, common.Address{0xbb}
	// The candidate's coinbase may mine one block in two ancestors and itself
	// per 100 locked stakes.
	config := &params.PoSWConfig{StakingEnabled: true, WindowSize: 3, StakePerBlock: big.NewInt(100), DiffDivider: 100}
	for _, test := range []struct {
		name   string
		chain  []common.Address
		stakes uint64
		want   int64
	}{
		{"no stake", []common.Address{{}, b, b}, 99, 100},
		{"quota unused", []common.Address{{}, b, b}, 100, 1},
		{"quota used", []common.Address{{}, a, b}, 100, 100},
		{"quota left", []common.Address{{}, a, b}, 200, 1},
		{"older blocks outside window", []common.Address{{}, a, b, b}, 100, 1},
		{"genesis counts", []common.Address{a}, 100, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain := newTestHeaderChain(test.chain...)
			header := chain.candidate(a)
			stakes := &testStakes{stakes: test.stakes}
			got, err := AdjustedShardDifficulty(config, chain, stakes, header)
			if err != nil {
				t.Fatal(err)
			}
			if got.Int64() != test.want {
				t.Fatalf("difficulty %v, want %d", got, test.want)
			}
			if stakes.at != chain.CurrentHeader() {
				t.Fatal("stakes were not read at the parent state")
			}
		})
	}
	chain := newTestHeaderChain(common.Address{}, b, b)
	failing := &testStakes{err: errors.New("no state")}
	// Without staking, neither stakes nor ancestors are read.
	for _, c := range []*params.PoSWConfig{nil, {StakingEnabled: false}} {
		if got, err := AdjustedShardDifficulty(c, chain, failing, chain.candidate(a)); err != nil || got.Int64() != 100 {
			t.Fatalf("staking disabled: %v, %v", got, err)
		}
	}
	genesis := chain.headers[0]
	if got, err := AdjustedShardDifficulty(config, chain, failing, genesis); err != nil || got.Sign() != 0 {
		t.Fatalf("genesis: %v, %v", got, err)
	}
	if _, err := AdjustedShardDifficulty(config, chain, failing, chain.candidate(a)); !errors.Is(err, failing.err) {
		t.Fatalf("stakes error: %v", err)
	}
	orphan := chain.candidate(a)
	orphan.ParentHash = common.Hash{1}
	if _, err := AdjustedShardDifficulty(config, chain, &testStakes{}, orphan); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("unknown parent: %v", err)
	}
	chain.missing[1] = true
	if _, err := AdjustedShardDifficulty(config, chain, &testStakes{}, chain.candidate(a)); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("missing ancestor in window: %v", err)
	}
}
