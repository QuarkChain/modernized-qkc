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

package genesis

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

func exampleInputs(t *testing.T) *config.ClusterConfig {
	t.Helper()
	cluster, err := config.LoadCluster("../config/examples/cluster.toml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entries := []*config.NodeConfig{&cluster.Master}
	for i := range cluster.Slaves {
		entries = append(entries, &cluster.Slaves[i])
	}
	for _, entry := range entries {
		data, err := os.ReadFile(entry.GenesisFile)
		if err != nil {
			t.Fatal(err)
		}
		entry.GenesisFile = filepath.Join(dir, filepath.Base(entry.GenesisFile))
		writeFile(t, entry.GenesisFile, data)
		entry.Node.DataDir = filepath.Join(dir, entry.Node.Name, "must-not-exist")
	}
	for i, slave := range cluster.Slaves {
		g := readShard(t, slave.GenesisFile)
		address := common.HexToAddress("0x1111111111111111111111111111111111111111")
		if i == 1 {
			address = common.HexToAddress("0x2222222222222222222222222222222222222222")
		}
		g.Alloc[address] = types.Account{Balance: big.NewInt(params.Ether)}
		writeJSON(t, slave.GenesisFile, g)
	}
	return cluster
}

func writeFile(t *testing.T, file string, data []byte) {
	t.Helper()
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, file string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, data)
}

func readShard(t *testing.T, file string) *core.Genesis {
	t.Helper()
	var g core.Genesis
	if err := decodeJSON(file, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func updateRootConfig(t *testing.T, file string, change func(*config.RootConfig)) {
	t.Helper()
	var input map[string]json.RawMessage
	if err := decodeJSON(file, &input); err != nil {
		t.Fatal(err)
	}
	var cfg config.RootConfig
	if err := json.Unmarshal(input["config"], &cfg); err != nil {
		t.Fatal(err)
	}
	change(&cfg)
	encoded, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	input["config"] = encoded
	writeJSON(t, file, input)
}

func TestLoadExample(t *testing.T) {
	cluster := exampleInputs(t)
	if err := cluster.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(cluster)
	files := make(map[string][]byte)
	for _, file := range []string{cluster.Master.GenesisFile, cluster.Slaves[0].GenesisFile, cluster.Slaves[1].GenesisFile} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		files[file] = data
	}
	generated, err := Load(cluster)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(cluster)
	if !bytes.Equal(before, after) {
		t.Fatal("loader changed the caller's deployment or loaded genesis values")
	}
	for file, before := range files {
		after, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("loader changed genesis file %s: %v", file, err)
		}
	}
	if len(generated.Shards) != 2 || generated.Root.NumberU64() != 0 {
		t.Fatal("unexpected network genesis")
	}
	headers := make([]*types.Header, len(generated.Shards))
	for i, result := range generated.Shards {
		h := result.Block.Header()
		headers[i] = h
		if result.Genesis.Config.ChainID.Cmp(cluster.Slaves[i].Eth.Genesis.Config.ChainID) != 0 || h.QKCDifficulty().Cmp(cluster.Slaves[i].Eth.Genesis.Config.QuarkChain.PoSW.MinimumDifficulty) != 0 {
			t.Fatal("network identity or difficulty was not applied")
		}
		extra, err := h.QKCExtra()
		if err != nil {
			t.Fatal(err)
		}
		if extra.PrevRootBlock != (common.Hash{}) || extra.Cursor != (types.XShardCursor{RootBlockHeight: 1}) || extra.XShardGasLimit != h.GasLimit/2 {
			t.Fatalf("incorrect genesis bootstrap metadata: %+v", extra)
		}
		if result.Block.Hash() != result.Genesis.ToBlock().Hash() || h.BaseFee.Cmp(new(big.Int).SetUint64(params.InitialBaseFee)) != 0 {
			t.Fatal("genesis specification does not reproduce the block")
		}
		if err := result.Genesis.ValidatePoSWStakingContracts(); err != nil {
			t.Fatal(err)
		}
		funded := common.HexToAddress("0x1111111111111111111111111111111111111111")
		if i == 1 {
			funded = common.HexToAddress("0x2222222222222222222222222222222222222222")
		}
		if result.Genesis.Alloc[funded].Balance.Cmp(big.NewInt(params.Ether)) != 0 {
			t.Fatal("initial allocation balance was changed")
		}
		_, hasRootStaking := result.Genesis.Alloc[params.RootStakingContractAddress]
		if hasRootStaking != (i == 0) {
			t.Fatal("root staking deployed on the wrong shard")
		}
		if _, err := os.Stat(cluster.Slaves[i].Node.DataDir); !os.IsNotExist(err) {
			t.Fatalf("generation touched datadir: %v", err)
		}
		// The native chain and shard bootstrap accept the generated definitions.
		db := rawdb.NewMemoryDatabase()
		engine, err := posw.New(result.Genesis.Config, h)
		if err != nil {
			t.Fatal(err)
		}
		chain, err := core.NewBlockChain(db, result.Genesis, engine, nil)
		if err != nil {
			t.Fatal(err)
		}
		sc := shard.NewShardChain(db, chain, nil)
		err = sc.Init(generated.Root)
		chain.Stop()
		if err != nil {
			t.Fatalf("bootstrap rejected generated genesis: %v", err)
		}
		// Native reopening restores the persisted full rules without another input genesis.
		reopened, err := core.NewBlockChain(db, nil, engine, nil)
		if err != nil {
			t.Fatalf("reopen generated genesis: %v", err)
		}
		if reopened.CurrentBlock().Hash() != result.Block.Hash() || !reopened.Config().QuarkChain.Equal(result.Genesis.Config.QuarkChain) {
			t.Fatal("reopening lost the genesis or its consensus rules")
		}
		reopened.Stop()
		engine.Close()
		db.Close()
	}
	if generated.Root.Header().ShardHeaderHash != qkctypes.CalcShardHeaderHash(headers) {
		t.Fatal("root does not commit to the shard genesis headers")
	}
	if _, err := os.Stat(cluster.Master.Node.DataDir); !os.IsNotExist(err) {
		t.Fatalf("generation touched Master datadir: %v", err)
	}
	again, err := Load(cluster)
	if err != nil || generated.Root.Hash() != again.Root.Hash() {
		t.Fatalf("generation is not deterministic: %v", err)
	}
	// Full native JSON definitions, including generated allocations and config,
	// can be read back without changing any genesis block hash.
	for i, result := range generated.Shards {
		writeJSON(t, cluster.Slaves[i].GenesisFile, result.Genesis)
	}
	roundtrip, err := Load(cluster)
	if err != nil || roundtrip.Root.Hash() != generated.Root.Hash() {
		t.Fatalf("complete JSON roundtrip failed: %v", err)
	}
	for i := range generated.Shards {
		if roundtrip.Shards[i].Block.Hash() != generated.Shards[i].Block.Hash() {
			t.Fatal("shard JSON roundtrip changed the hash")
		}
		before, _ := json.Marshal(generated.Shards[i].Genesis.Config)
		after, _ := json.Marshal(roundtrip.Shards[i].Genesis.Config)
		if !bytes.Equal(before, after) {
			t.Fatal("shard JSON roundtrip changed the full consensus rules")
		}
	}
	// The bundle retains independent full rules for later DB compatibility checks.
	cluster.Master.Root.PoSW.MinimumDifficulty.SetInt64(2)
	cluster.Master.Root.PoSW.DecayFactor.SetInt64(0)
	cluster.Slaves[0].Eth.Genesis.Config.QuarkChain.PoSW.MinimumDifficulty.SetInt64(2)
	cluster.Slaves[0].Eth.Genesis.Config.QuarkChain.ChainIDs[0].SetInt64(7)
	if generated.RootConfig.PoSW.MinimumDifficulty.Int64() != 1 || generated.RootConfig.PoSW.DecayFactor.Sign() == 0 || generated.Shards[0].Genesis.Config.QuarkChain.PoSW.MinimumDifficulty.Int64() != 1 || generated.Shards[0].Genesis.Config.QuarkChain.ChainIDs[0].Int64() != 100001 {
		t.Fatal("bundle shares mutable caller configuration")
	}
}

func TestLoadOrderAndMetadata(t *testing.T) {
	cluster := exampleInputs(t)
	baseline, err := Load(cluster)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(cluster.Slaves)
	cluster.Master.CoinbaseAddress = common.Address{0xff}
	cluster.Master.Node.DataDir = filepath.Join(t.TempDir(), "unused-master")
	cluster.Master.Node.HTTPPort = -1
	for i := range cluster.Slaves {
		cluster.Slaves[i].CoinbaseAddress = common.Address{0xee}
		cluster.Slaves[i].PrivatePort = 0
		cluster.Slaves[i].Node.DataDir = filepath.Join(t.TempDir(), "unused-shard")
	}
	again, err := Load(cluster)
	if err != nil || baseline.Root.Hash() != again.Root.Hash() {
		t.Fatalf("deployment order, ports, paths or mining recipients affected genesis: %v", err)
	}
	updateRootConfig(t, cluster.Master.GenesisFile, func(c *config.RootConfig) { c.PoSW.CoinbaseAmount.SetInt64(100) })
	first := readShard(t, cluster.Slaves[1].GenesisFile)
	first.Config.QuarkChain.PoSW.CoinbaseAmount.SetInt64(100)
	writeJSON(t, cluster.Slaves[1].GenesisFile, first)
	changed, err := Load(cluster)
	if err != nil || changed.Root.Hash() != baseline.Root.Hash() {
		t.Fatalf("unexpected hash change for consensus metadata: %v", err)
	}
	if changed.RootConfig.PoSW.CoinbaseAmount.Int64() != 100 || changed.Shards[0].Genesis.Config.QuarkChain.PoSW.CoinbaseAmount.Int64() != 100 {
		t.Fatal("consensus settings were dropped because block hashes matched")
	}
	for _, slave := range cluster.Slaves {
		g := readShard(t, slave.GenesisFile)
		slices.Reverse(g.Config.QuarkChain.ChainIDs)
		delete(g.Alloc, params.RootStakingContractAddress)
		writeJSON(t, slave.GenesisFile, g)
	}
	reordered, err := Load(cluster)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Root.Hash() == baseline.Root.Hash() || reordered.Shards[0].Genesis.Config.ChainID.Int64() != 100002 {
		t.Fatal("genesis consensus order was ignored")
	}
	if len(reordered.Shards[0].Genesis.Alloc[params.RootStakingContractAddress].Code) == 0 || len(reordered.Shards[1].Genesis.Alloc[params.RootStakingContractAddress].Code) != 0 {
		t.Fatal("root staking did not follow the first genesis shard")
	}
}

func TestLoadCoverage(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*config.ClusterConfig)
		want   string
	}{
		{"root file", func(c *config.ClusterConfig) { c.Master.GenesisFile = "" }, "Master.GenesisFile"},
		{"shard file", func(c *config.ClusterConfig) { c.Slaves[0].GenesisFile = "" }, "GenesisFile is required"},
		{"missing chain", func(c *config.ClusterConfig) { c.Slaves = c.Slaves[:1] }, "every"},
		{"duplicate chain", func(c *config.ClusterConfig) {
			c.Slaves[1].ChainID = c.Slaves[0].ChainID
			c.Slaves[1].GenesisFile = c.Slaves[0].GenesisFile
		}, "duplicate"},
		{"unknown chain", func(c *config.ClusterConfig) { c.Slaves[0].ChainID = big.NewInt(7) }, "ChainID"},
		{"nil chain", func(c *config.ClusterConfig) { c.Slaves[0].ChainID = nil }, "ChainID"},
		{"missing file", func(c *config.ClusterConfig) { c.Slaves[0].GenesisFile += ".missing" }, ".missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := exampleInputs(t)
			test.change(c)
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("missing configuration accepted")
	}
}

func TestLoadFullUint256ChainID(t *testing.T) {
	c := exampleInputs(t)
	id := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	c.Slaves[1].ChainID = new(big.Int).Set(id)
	for i, slave := range c.Slaves {
		g := readShard(t, slave.GenesisFile)
		if i == 1 {
			g.Config.ChainID = new(big.Int).Set(id)
			g.ExtraData = nil
		}
		g.Config.QuarkChain.ChainIDs[1] = new(big.Int).Set(id)
		writeJSON(t, slave.GenesisFile, g)
	}
	generated, err := Load(c)
	if err != nil {
		t.Fatal(err)
	}
	extra, _ := generated.Shards[1].Block.Header().QKCExtra()
	if extra.ChainID.Cmp(id) != 0 {
		t.Fatal("chain ID was truncated")
	}
}
