// Copyright 2026-2027 The quarkchain Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	qkcconfig "github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/genesis"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/urfave/cli/v2"
)

func initExample(t *testing.T) string {
	t.Helper()
	file := copyQKCExample(t)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("./qkc-data"), []byte(filepath.ToSlash(filepath.Join(filepath.Dir(file), "data"))))
	data = bytes.ReplaceAll(data, []byte("DatabaseCache = 512"), []byte("DatabaseCache = 16"))
	data = bytes.ReplaceAll(data, []byte("DatabaseCache = 256"), []byte("DatabaseCache = 16"))
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "IPCPath =") {
			lines[i] = ""
		}
	}
	data = []byte(strings.Join(lines, "\n"))
	for _, section := range []string{"[Master.Node]", "[Slaves.Node]"} {
		data = bytes.ReplaceAll(data, []byte(section), []byte(section+"\nIPCPath = \"\""))
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func runInit(args ...string) (string, error) {
	var out bytes.Buffer
	app := &cli.App{Commands: []*cli.Command{initQKCCommand}, Writer: &out, ErrWriter: &out}
	err := app.Run(append([]string{"qkc", "init"}, args...))
	return out.String(), err
}

func openInitDatabase(t *testing.T, cfg *qkcconfig.NodeConfig) (*node.Node, ethdb.Database) {
	t.Helper()
	stack, err := node.New(&cfg.Node)
	if err != nil {
		t.Fatal(err)
	}
	name := "rootdata"
	if cfg.Eth != nil {
		name = "chaindata"
	}
	db, err := stack.OpenDatabaseWithOptions(name, node.DatabaseOptions{Cache: 16, Handles: 16})
	if err != nil {
		stack.Close()
		t.Fatal(err)
	}
	return stack, db
}

func TestInitPersistence(t *testing.T) {
	for _, engine := range []string{rawdb.DBPebble, rawdb.DBLeveldb} {
		for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
			t.Run(engine+"/"+scheme, func(t *testing.T) {
				file := initExample(t)
				cluster, err := qkcconfig.LoadCluster(file)
				if err != nil {
					t.Fatal(err)
				}
				var spec core.Genesis
				data, _ := os.ReadFile(cluster.Slaves[0].GenesisFile)
				if err := json.Unmarshal(data, &spec); err != nil {
					t.Fatal(err)
				}
				address := common.Address{0x77}
				spec.Alloc[address] = types.Account{Balance: big.NewInt(42), Nonce: 4, Code: []byte{0x60, 0}, Storage: map[common.Hash]common.Hash{{1}: {2}}}
				data, _ = json.Marshal(&spec)
				if err := os.WriteFile(cluster.Slaves[0].GenesisFile, data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"--config", file, "--db.engine", engine, "--state.scheme", scheme}
				if _, err := runInit(args...); err != nil {
					t.Fatal(err)
				}
				if out, err := runInit(args...); err != nil || strings.Count(out, "Initialized ") != 3 {
					t.Fatalf("repeat init: %v, output %s", err, out)
				}
				if err := cluster.LoadGenesis(); err != nil {
					t.Fatal(err)
				}
				bundle, err := genesis.Build(cluster)
				if err != nil {
					t.Fatal(err)
				}
				for _, cfg := range append([]qkcconfig.NodeConfig{cluster.Master}, cluster.Slaves...) {
					stack, db := openInitDatabase(t, &cfg)
					if cfg.Eth == nil {
						if hash, err := bundle.InitRootDatabase(db); err != nil || hash != bundle.Root.Hash() {
							t.Fatalf("reopen root database: %v", err)
						}
					} else {
						iterator := db.NewIterator([]byte("qkc_"), nil)
						hasMetadata := iterator.Next()
						iterator.Release()
						if hasMetadata {
							t.Fatal("local shard initialization wrote root or extra QKC metadata")
						}
					}
					if cfg.Eth != nil {
						stored, err := core.ReadGenesis(db)
						if err != nil {
							t.Fatal(err)
						}
						pow, err := posw.New(stored.Config, stored.ToBlock().Header())
						if err != nil {
							t.Fatal(err)
						}
						chain, err := core.NewBlockChain(db, stored, pow, core.DefaultConfig().WithStateScheme(scheme))
						if err != nil {
							t.Fatal(err)
						}
						state, err := chain.State()
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(state.GetCode(params.ShardStakingContractAddress), params.PoSWStakingRuntimeCode) {
							t.Fatal("staking contract state not persisted")
						}
						if cfg.ChainID.Int64() == 100001 && (state.GetBalance(address).Uint64() != 42 || state.GetNonce(address) != 4 || !bytes.Equal(state.GetCode(address), []byte{0x60, 0}) || state.GetState(address, common.Hash{1}) != (common.Hash{2})) {
							t.Fatal("allocation balance, nonce, code or storage not persisted")
						}
						chain.Stop()
						pow.Close()
					}
					if err := stack.Close(); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestInitSelectionAndRetry(t *testing.T) {
	file := initExample(t)
	if _, err := runInit("--config", file, "--qkc.role", "slave", "--qkc.chain", "100002"); err != nil {
		t.Fatal(err)
	}
	cluster, err := qkcconfig.LoadCluster(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.Master.Node.DataDir); !os.IsNotExist(err) {
		t.Fatal("selecting one shard created master's datadir")
	}
	stack, _ := openInitDatabase(t, &cluster.Slaves[1])
	if _, err := runInit("--config", file); err == nil {
		t.Fatal("initialization ignored database lock")
	}
	stack.Close()
	// A later failure leaves the already initialized processes usable.
	master, rootdb := openInitDatabase(t, &cluster.Master)
	if hash, err := qkcrawdb.ReadRootGenesisHash(rootdb); err != nil || hash == (common.Hash{}) {
		t.Fatal("earlier root initialization was not retained")
	}
	master.Close()
	if out, err := runInit("--config", file); err != nil || strings.Count(out, "Initialized ") != 3 {
		t.Fatalf("resume partial deployment: %v, output %s", err, out)
	}
	// Like geth, P2P network ID is a process setting, not a second chain config.
	content, _ := os.ReadFile(file)
	content = bytes.Replace(content, []byte("NetworkID = 2"), []byte("NetworkID = 3"), 1)
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runInit("--config", file); err != nil {
		t.Fatalf("P2P network ID changed genesis compatibility: %v", err)
	}
}

func TestInitSlaveUsesOnlyLocalGenesis(t *testing.T) {
	file := initExample(t)
	cluster, err := qkcconfig.LoadCluster(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{cluster.Master.GenesisFile, cluster.Slaves[0].GenesisFile} {
		if err := os.Remove(missing); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runInit("--config", file, "--qkc.role", "slave", "--qkc.chain", "100002"); err != nil {
		t.Fatalf("local slave init tried to load another chain: %v", err)
	}
}

func TestInitSlaveNativeCompatibility(t *testing.T) {
	file := initExample(t)
	cluster, err := qkcconfig.LoadCluster(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &cluster.Slaves[0]
	if err := cfg.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	spec := cfg.Eth.Genesis
	timestamp := uint64(100)
	spec.Config.ShanghaiTime = &timestamp
	save := func() {
		t.Helper()
		data, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.GenesisFile, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	// An existing native geth-initialized database has no QKC init markers.
	stack, db := openInitDatabase(t, cfg)
	trie := triedb.NewDatabase(db, nil)
	_, hash, compatErr, err := core.SetupGenesisBlock(db, trie, spec)
	if err != nil || compatErr != nil {
		t.Fatalf("native init: %v, %v", err, compatErr)
	}
	trie.Close()
	stack.Close()
	args := []string{"--config", file, "--qkc.role", "slave", "--qkc.chain", "100001", "--state.scheme", "hash"}
	if _, err := runInit(args...); err != nil {
		t.Fatalf("matching native database rejected: %v", err)
	}
	timestamp = 200
	save()
	if _, err := runInit(args...); err != nil {
		t.Fatalf("compatible future fork rejected: %v", err)
	}
	stack, db = openInitDatabase(t, cfg)
	stored := rawdb.ReadChainConfig(db, hash)
	if stored == nil || stored.ShanghaiTime == nil || *stored.ShanghaiTime != 200 {
		t.Fatal("native compatible fork update not persisted")
	}
	stack.Close()
	spec.Config.QuarkChain.PoSW.CoinbaseAmount.SetInt64(123)
	save()
	if _, err := runInit(args...); err == nil {
		t.Fatal("native QKC consensus compatibility check was bypassed")
	}
}

func TestInitRejectsInvalidDeploymentBeforeWriting(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "deployment"},
		{name: "master", args: []string{"--qkc.role", "master"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := initExample(t)
			content, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			content = bytes.Replace(content, []byte("NetworkID = 2"), []byte("NetworkID = 0"), 1)
			if err := os.WriteFile(file, content, 0600); err != nil {
				t.Fatal(err)
			}
			cluster, err := qkcconfig.LoadCluster(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runInit(append([]string{"--config", file}, test.args...)...); err == nil || !strings.Contains(err.Error(), "NetworkID must be positive") {
				t.Fatalf("invalid deployment accepted: %v", err)
			}
			for _, cfg := range append([]qkcconfig.NodeConfig{cluster.Master}, cluster.Slaves...) {
				if _, err := os.Stat(cfg.Node.DataDir); !os.IsNotExist(err) {
					t.Fatalf("invalid deployment created data directory %s: %v", cfg.Node.DataDir, err)
				}
			}
		})
	}
}

func TestInitArguments(t *testing.T) {
	for _, args := range [][]string{
		{"extra"},
		{"--qkc.chain", "100001"},
		{"--datadir", "data"},
		{"--config", "missing.toml"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			if _, err := runInit(args...); err == nil {
				t.Fatal("invalid init arguments accepted")
			}
		})
	}
}
