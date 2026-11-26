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

package root_test

import (
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/root"
)

func TestDevnetRootGenesisHash(t *testing.T) {
	cfg, err := config.LoadRoot(filepath.Join("..", "config", "devnet", "root.toml"))
	if err != nil {
		t.Fatal(err)
	}
	genesis, err := root.BuildRootGenesis(cfg.Genesis, cfg.ShardGeneses())
	if err != nil {
		t.Fatal(err)
	}
	want := common.HexToHash("0xf485305f0b97809fda06ee5ddffeb0444e1c9035b8de82bf5185bde191746646")
	if genesis.Hash() != want {
		t.Fatalf("devnet genesis hash %s, want %s", genesis.Hash(), want)
	}
}

func TestBuildRootGenesisChainIDOrder(t *testing.T) {
	var first common.Hash
	for i := 0; i < 2; i++ {
		cfg, err := config.LoadRoot(filepath.Join("..", "config", "devnet", "root.toml"))
		if err != nil {
			t.Fatal(err)
		}
		// Put the root staking contract on the newly designated first chain as well.
		shards := cfg.ShardGeneses()
		staking := params.RootStakingContractAddress
		shards[1].Alloc[staking] = shards[0].Alloc[staking]
		delete(shards[0].Alloc, staking)
		slices.Reverse(shards)
		ids := slices.Clone(cfg.Genesis.Config.ChainIDs)
		slices.Reverse(ids)
		cfg.Genesis.Config.ChainIDs = ids
		for _, shard := range shards {
			shard.Config.QuarkChain.ChainIDs = ids
		}
		wantHeaders := []common.Hash{shards[1].ToBlock().Hash(), shards[0].ToBlock().Hash()}
		genesis, err := root.BuildRootGenesis(cfg.Genesis, shards)
		if err != nil {
			t.Fatal(err)
		}
		for j, want := range wantHeaders {
			if got := genesis.ShardBlockHeaders()[j].Hash(); got != want {
				t.Fatalf("G0 at index %d: got %s want %s", j, got, want)
			}
		}
		if i == 0 {
			first = genesis.Hash()
		} else if genesis.Hash() != first {
			t.Fatalf("independent build produced %s, want %s", genesis.Hash(), first)
		}
	}
}

func TestBuildRootGenesisInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*config.RootConfig)
	}{
		{"missing genesis", "missing root genesis", func(c *config.RootConfig) { c.Genesis = nil }},
		{"zero difficulty", "positive uint256", func(c *config.RootConfig) { c.Genesis.Difficulty = new(big.Int) }},
		{"difficulty below minimum", "below MinimumDifficulty", func(c *config.RootConfig) { c.Genesis.Difficulty = big.NewInt(1023) }},
		{"shard count", "shard genesis count", func(c *config.RootConfig) { c.Shards = c.Shards[:1] }},
		{"shard order", "expected shard genesis for chain", func(c *config.RootConfig) { slices.Reverse(c.Shards) }},
		{"late shard genesis", "shard 100002 genesis timestamp 1 is later than root genesis timestamp 0", func(c *config.RootConfig) {
			c.Shards[1].Eth.Genesis.Timestamp = c.Genesis.Timestamp + 1
		}},
		{"network rules", "network rules differ", func(c *config.RootConfig) { c.Shards[1].Eth.Genesis.Config.QuarkChain.MaxBlocksPerRootBlock++ }},
		{"invalid shard genesis", "invalid gas limit", func(c *config.RootConfig) { c.Shards[0].Eth.Genesis.GasLimit = 10000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.LoadRoot(filepath.Join("..", "config", "devnet", "root.toml"))
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(cfg)
			if _, err := root.BuildRootGenesis(cfg.Genesis, cfg.ShardGeneses()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
