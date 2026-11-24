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

package root_test

import (
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/root"
)

func TestDevnetRootGenesisRecovery(t *testing.T) {
	cfg, err := config.LoadRoot(filepath.Join("..", "config", "devnet", "root.toml"))
	if err != nil {
		t.Fatal(err)
	}
	seed, shards := cfg.Genesis, cfg.ShardGeneses()
	genesis, err := root.BuildRootGenesis(seed, shards)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	disk, err := pebble.New(dir, 0, 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(disk)
	_, hash, err := root.InitializeRootDatabase(db, seed, shards)
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if want := genesis.Hash(); hash != want {
		t.Fatalf("devnet genesis hash %s, want %s", hash, want)
	}

	// The database alone must be enough to recover the local chain and its rules.
	disk, err = pebble.New(dir, 0, 0, "", true)
	if err != nil {
		t.Fatal(err)
	}
	db = rawdb.NewDatabase(disk)
	defer db.Close()
	rules, recovered, err := root.SetupRootGenesisBlock(db, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != hash || !rules.Equal(seed.Config) {
		t.Fatalf("recovered hash %s and rules %+v, want %s and %+v", recovered, rules, hash, seed.Config)
	}
	loaded, restored, err := root.InitializeRootDatabase(db, nil, shards)
	if err != nil || restored != hash || !loaded.Equal(rules) {
		t.Fatalf("reopening with shard configs only: hash %s, rules %+v, err %v", restored, loaded, err)
	}
	engine, err := posw.NewSimulate(rules)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	// A nil embedded client fails the test if local recovery calls cluster RPCs.
	client := struct{ root.ClusterClient }{}
	chain, err := root.New(t.Context(), rules, client, db, engine)
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Close()
	if tip := chain.RootTip(); tip.Number != 0 || tip.Hash() != hash {
		t.Fatalf("recovered root tip %d/%s, want 0/%s", tip.Number, tip.Hash(), hash)
	}
	block, err := chain.GetRootBlockByNumber(0)
	if err != nil || block == nil || block.Hash() != hash {
		t.Fatalf("canonical genesis lookup returned %v, %v", block, err)
	}
	confirmations, err := chain.GetLastConfirmedShardBlocks(hash)
	if err != nil {
		t.Fatal(err)
	}
	headers := block.ShardBlockHeaders()
	if len(headers) != 2 || len(confirmations) != 2 || len(shards) != 2 {
		t.Fatalf("devnet shard count: %d headers, %d confirmations, %d configurations", len(headers), len(confirmations), len(shards))
	}
	for i, shard := range shards {
		id := shard.Config.ChainID.String()
		want := shard.ToBlock().Hash()
		if got := headers[i].Hash(); got != want {
			t.Errorf("shard %s genesis header hash %s, want %s", id, got, want)
		}
		if got := confirmations[id]; got != want {
			t.Errorf("shard %s confirmed hash %s, want %s", id, got, want)
		}
	}
	t.Run("changed allocation", func(t *testing.T) {
		alloc := shards[0].Alloc
		address := common.Address{1}
		alloc[address] = types.Account{Balance: big.NewInt(1)}
		defer delete(alloc, address)
		if _, _, err := root.InitializeRootDatabase(db, nil, shards); err == nil {
			t.Fatal("accepted shard genesis with a different state root")
		}
	})
	t.Run("changed shared rules", func(t *testing.T) {
		for _, shard := range shards {
			shard.Config.QuarkChain.MaxBlocksPerRootBlock++
		}
		if _, _, err := root.InitializeRootDatabase(db, nil, shards); err == nil {
			t.Fatal("accepted different network rules with the same shard genesis headers")
		}
	})
}
