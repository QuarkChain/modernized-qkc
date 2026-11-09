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

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/naoina/toml"
)

func writeConfig(t *testing.T, content []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func exampleCluster(t *testing.T) *ClusterConfig {
	t.Helper()
	cfg, err := LoadCluster("examples/cluster.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func copyExampleCluster(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "genesis"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"cluster.toml", "genesis/root.json", "genesis/shard-100001.json", "genesis/shard-100002.json"} {
		data, err := os.ReadFile(filepath.Join("examples", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "cluster.toml")
}

func TestGenesisConfiguration(t *testing.T) {
	cfg := exampleCluster(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkID != 2 || len(cfg.Slaves) != 2 {
		t.Fatalf("unexpected deployment: %+v", cfg)
	}
	root := cfg.Master.Root
	first, second := cfg.Slaves[0].Eth.Genesis.Config, cfg.Slaves[1].Eth.Genesis.Config
	if root.ConsensusType != params.ConsensusEthash || first.QuarkChain.ConsensusType != params.ConsensusEthash || second.QuarkChain.ConsensusType != params.ConsensusSimulate {
		t.Fatal("root and shard consensus algorithms were not preserved")
	}
	if !root.PoSW.StakingEnabled || first.QuarkChain.PoSW.StakingEnabled || !second.QuarkChain.PoSW.StakingEnabled {
		t.Fatal("root and shard PoSW configurations are not independent")
	}
	if root.PoSW.StakePerBlock.String() != "1000000000000000000000000000000" {
		t.Fatal("large stake was not preserved")
	}
	if root.PoSW.DecayFactor.Cmp(big.NewRat(22, 25)) != 0 || first.QuarkChain.RewardTaxRate.Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatal("consensus ratios were not parsed exactly")
	}
	for _, slave := range cfg.Slaves {
		chain := slave.Eth.Genesis.Config
		if !reflect.DeepEqual(chain.QuarkChain.ChainIDs, []*big.Int{big.NewInt(100001), big.NewInt(100002)}) {
			t.Fatalf("unexpected genesis topology: %v", chain.QuarkChain.ChainIDs)
		}
		if chain.QuarkChain.MaxBlocksPerRootBlock != 18 {
			t.Fatal("configured shard confirmation limit was not preserved")
		}
	}
	first.QuarkChain.ChainIDs[1].SetInt64(7)
	if second.ChainID.Int64() != 100002 || second.QuarkChain.ChainIDs[1].Int64() != 100002 {
		t.Fatal("genesis values alias another shard or its identity")
	}
}

func validateGenesisForTest(c *ClusterConfig) error {
	for _, node := range append([]NodeConfig{c.Master}, c.Slaves...) {
		if err := node.validateGenesisConfig(); err != nil {
			return err
		}
	}
	return c.validateGenesisMembership()
}

func TestGenesisValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ClusterConfig)
		want   string
	}{
		{"missing root source", func(c *ClusterConfig) { c.Master.GenesisFile, c.Master.Root = "", nil }, "GenesisFile"},
		{"missing shard source", func(c *ClusterConfig) { c.Slaves[0].GenesisFile, c.Slaves[0].Eth.Genesis = "", nil }, "GenesisFile"},
		{"missing root config", func(c *ClusterConfig) { c.Master.Root = nil }, "config"},
		{"missing root consensus", func(c *ClusterConfig) { c.Master.Root.ConsensusType = "" }, "invalid ConsensusType"},
		{"invalid root consensus", func(c *ClusterConfig) { c.Master.Root.ConsensusType = "NONE" }, "invalid ConsensusType"},
		{"unsupported root consensus", func(c *ClusterConfig) { c.Master.Root.ConsensusType = "POW_QKCHASH" }, "invalid ConsensusType"},
		{"missing root posw", func(c *ClusterConfig) { c.Master.Root.PoSW = nil }, "posw:"},
		{"invalid root staking", func(c *ClusterConfig) { c.Master.Root.PoSW.WindowSize = 0 }, "posw:"},
		{"negative root stake", func(c *ClusterConfig) { c.Master.Root.PoSW.StakePerBlock.SetInt64(-1) }, "posw:"},
		{"overflow root stake", func(c *ClusterConfig) { c.Master.Root.PoSW.StakePerBlock.Lsh(big.NewInt(1), 256) }, "posw:"},
		{"missing genesis", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis = nil }, "Genesis config"},
		{"missing chain config", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config = nil }, "config"},
		{"missing quarkchain", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain = nil }, "QuarkChain"},
		{"beacon deposit contract", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.DepositContractAddress[19] = 1 }, "DepositContractAddress must be zero"},
		{"missing shard consensus", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.ConsensusType = "" }, "invalid ConsensusType"},
		{"invalid shard consensus", func(c *ClusterConfig) { c.Slaves[1].Eth.Genesis.Config.QuarkChain.ConsensusType = "ethash" }, "invalid ConsensusType"},
		{"unsupported shard consensus", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.ConsensusType = "POW_DOUBLESHA256" }, "invalid ConsensusType"},
		{"missing shard posw", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.PoSW = nil }, "posw:"},
		{"invalid ordinary pow", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.PoSW.MinimumDifficulty = nil }, "posw:"},
		{"invalid shard staking", func(c *ClusterConfig) { c.Slaves[1].Eth.Genesis.Config.QuarkChain.PoSW.DiffDivider = 0 }, "posw:"},
		{"invalid fork order", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.HomesteadBlock.SetInt64(1) }, "unsupported fork ordering"},
		{"missing membership", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.ChainIDs = nil }, "chain IDs must not be empty"},
		{"duplicate member", func(c *ClusterConfig) { c.Slaves[0].Eth.Genesis.Config.QuarkChain.ChainIDs[1] = big.NewInt(100001) }, "duplicate chain ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleCluster(t)
			test.change(cfg)
			if err := validateGenesisForTest(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestGenesisValidationIgnoresRuntimeSettings(t *testing.T) {
	cfg := exampleCluster(t)
	cfg.NetworkID = 0
	cfg.Master.Node.HTTPPort = -1
	for _, slave := range cfg.Slaves {
		slave.Eth.NetworkId = 99
	}
	cfg.Slaves[0].PrivateHost = ""
	cfg.Slaves[0].PrivatePort = 0
	cfg.Slaves[1].Node.DataDir = cfg.Slaves[0].Node.DataDir
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGenesisForTest(cfg); err != nil {
		t.Fatalf("genesis validation depended on runtime settings: %v", err)
	}
	for _, node := range append([]NodeConfig{cfg.Master}, cfg.Slaves...) {
		if err := node.validateGenesisConfig(); err != nil {
			t.Fatalf("local genesis validation depended on runtime settings: %v", err)
		}
	}
	after, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("genesis validation modified the caller's configuration")
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("full validation accepted invalid deployment settings")
	}
	cfg.Slaves[1].Eth.Genesis.Config.QuarkChain.ChainIDs = []*big.Int{big.NewInt(100002), big.NewInt(100001)}
	if err := validateGenesisForTest(cfg); err == nil || !strings.Contains(err.Error(), "match in order") {
		t.Fatalf("genesis-only validation missed inconsistent membership: %v", err)
	}
}

func TestLoadClusterLeavesGenesisUnloaded(t *testing.T) {
	cfg, err := LoadCluster("examples/cluster.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Master.Root != nil {
		t.Fatal("deployment loading read root genesis")
	}
	for _, slave := range cfg.Slaves {
		if slave.Eth.Genesis != nil {
			t.Fatal("deployment loading read shard genesis")
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("deployment validation depended on genesis: %v", err)
	}
	if cfg.Master.Root != nil || cfg.Slaves[0].Eth.Genesis != nil || cfg.Slaves[1].Eth.Genesis != nil {
		t.Fatal("validation loaded genesis files")
	}
	for _, entry := range []*NodeConfig{&cfg.Master, &cfg.Slaves[0], &cfg.Slaves[1]} {
		path := entry.GenesisFile
		if filepath.Dir(path) != filepath.Join("examples", "genesis") {
			t.Fatalf("genesis path not resolved relative to deployment: %s", path)
		}
		for range 2 {
			if err := entry.LoadGenesis(); err != nil {
				t.Fatal(err)
			}
			if entry.GenesisFile != path {
				t.Fatalf("loading genesis resolved its path again: have %s, want %s", entry.GenesisFile, path)
			}
		}
	}
}

func TestSelectedGenesisLoading(t *testing.T) {
	for _, test := range []struct {
		name, file, label string
		missing           bool
	}{
		{"missing root", "root.json", "Master", true},
		{"malformed root", "root.json", "Master", false},
		{"missing peer", "shard-100002.json", "Slaves[1]", true},
		{"malformed peer", "shard-100002.json", "Slaves[1]", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := copyExampleCluster(t)
			broken := filepath.Join(filepath.Dir(file), "genesis", test.file)
			if test.missing {
				if err := os.Remove(broken); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(broken, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadCluster(file)
			if err != nil {
				t.Fatalf("deployment loading depended on an unselected genesis: %v", err)
			}
			selected, err := cfg.SelectNode(RoleSlave, big.NewInt(100001))
			if err != nil {
				t.Fatal(err)
			}
			if err := selected.LoadGenesis(); err != nil {
				t.Fatalf("selected genesis loading depended on another process: %v", err)
			}
			if err := selected.Validate(cfg.NetworkID); err != nil {
				t.Fatalf("selected node validation depended on another process: %v", err)
			}
			if cfg.Master.Root != nil || cfg.Slaves[1].Eth.Genesis != nil {
				t.Fatal("loading or validating one process read another process's genesis")
			}
			if err := cfg.LoadGenesis(); err == nil || !strings.Contains(err.Error(), broken) || !strings.Contains(err.Error(), test.label) {
				t.Fatalf("whole-cluster loading should report the broken genesis and its owner: %v", err)
			}
		})
	}
}

func TestGenesisLoading(t *testing.T) {
	for _, test := range []struct {
		name, file, content string
	}{
		{"invalid root JSON", "root.json", "{"},
		{"invalid shard JSON", "shard-100001.json", "{"},
		{"trailing root document", "root.json", "{} {}"},
		{"trailing shard document", "shard-100001.json", "{} {}"},
		{"missing shard fields", "shard-100001.json", "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := copyExampleCluster(t)
			genesis := filepath.Join(filepath.Dir(file), "genesis", test.file)
			if err := os.WriteFile(genesis, []byte(test.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadCluster(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.LoadGenesis(); err == nil || !strings.Contains(err.Error(), genesis) {
				t.Fatalf("invalid genesis accepted or error lacks its path: %v", err)
			}
		})
	}
	t.Run("missing referenced file", func(t *testing.T) {
		file := copyExampleCluster(t)
		genesis := filepath.Join(filepath.Dir(file), "genesis", "root.json")
		if err := os.Remove(genesis); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadCluster(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.LoadGenesis(); err == nil || !strings.Contains(err.Error(), genesis) {
			t.Fatalf("missing genesis accepted or error lacks its path: %v", err)
		}
	})
	t.Run("missing config is not supplied from elsewhere", func(t *testing.T) {
		file := copyExampleCluster(t)
		genesis := filepath.Join(filepath.Dir(file), "genesis", "shard-100001.json")
		data, err := os.ReadFile(genesis)
		if err != nil {
			t.Fatal(err)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		delete(spec, "config")
		data, err = json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(genesis, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadCluster(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.LoadGenesis(); err == nil || !strings.Contains(err.Error(), "config") {
			t.Fatalf("missing native genesis config was accepted: %v", err)
		}
	})
}

func TestGenesisRewardTaxRate(t *testing.T) {
	for _, test := range []struct {
		name       string
		rate       *big.Rat
		coinbase   int64
		staking    bool
		wantReward int64
		wantError  string
	}{
		{"zero", new(big.Rat), 100, true, 0, "quarkchain: reward tax rate"},
		{"small nonzero with zero subsidy", big.NewRat(1, 100), 0, true, 0, ""},
		{"half with staking disabled", big.NewRat(1, 2), 100, false, 50, ""},
		{"whole", big.NewRat(1, 1), 100, true, 0, "quarkchain: reward tax rate"},
		{"missing", nil, 100, true, 0, "quarkchain: reward tax rate"},
		{"negative", big.NewRat(-1, 2), 100, true, 0, "quarkchain: reward tax rate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleCluster(t)
			cfg.Master.Root.PoSW.CoinbaseAmount.SetInt64(100)
			shard := cfg.Slaves[1].Eth.Genesis.Config.QuarkChain
			shard.RewardTaxRate = test.rate
			shard.PoSW.CoinbaseAmount.SetInt64(test.coinbase)
			shard.PoSW.StakingEnabled = test.staking
			err := validateGenesisForTest(cfg)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("have %v, want error containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if reward := cfg.Master.Root.PoSW.BlockRewardAt(big.NewInt(1)); reward.Cmp(big.NewInt(100)) != 0 {
				t.Fatalf("root reward = %v, want 100", reward)
			}
			want := big.NewInt(test.wantReward)
			if reward := shard.AfterTax(shard.PoSW.BlockRewardAt(big.NewInt(1))); reward.Cmp(want) != 0 {
				t.Fatalf("shard reward = %v, want %v", reward, want)
			}
		})
	}
}

func TestGenesisSharedForks(t *testing.T) {
	forks := []string{
		"HomesteadBlock", "EIP150Block", "EIP155Block", "EIP158Block", "ByzantiumBlock",
		"ConstantinopleBlock", "PetersburgBlock", "IstanbulBlock", "BerlinBlock", "LondonBlock",
	}
	for _, fork := range forks {
		t.Run(fork, func(t *testing.T) {
			cfg := exampleCluster(t)
			// Spaced heights let one fork differ without breaking local fork order.
			for _, slave := range cfg.Slaves {
				for i, name := range forks {
					reflect.ValueOf(slave.Eth.Genesis.Config).Elem().FieldByName(name).Set(reflect.ValueOf(big.NewInt(int64((i + 1) * 10))))
				}
			}
			if err := validateGenesisForTest(cfg); err != nil {
				t.Fatalf("matching nonzero forks rejected: %v", err)
			}
			field := reflect.ValueOf(cfg.Slaves[1].Eth.Genesis.Config).Elem().FieldByName(fork).Interface().(*big.Int)
			field.Add(field, big.NewInt(1))
			if err := validateGenesisForTest(cfg); err == nil || !strings.Contains(err.Error(), fork) {
				t.Fatalf("different %s heights accepted: %v", fork, err)
			}
		})
	}
	cfg := exampleCluster(t)
	for _, slave := range cfg.Slaves {
		slave.Eth.Genesis.Config.LondonBlock = nil
	}
	if err := validateGenesisForTest(cfg); err != nil {
		t.Fatalf("matching omitted forks rejected: %v", err)
	}
	cfg.Slaves[1].Eth.Genesis.Config.LondonBlock = new(big.Int)
	if err := validateGenesisForTest(cfg); err == nil || !strings.Contains(err.Error(), "LondonBlock") {
		t.Fatalf("omitted and zero forks were treated as equal: %v", err)
	}
}

func TestGenesisRoutingOrder(t *testing.T) {
	cfg := exampleCluster(t)
	cfg.Slaves[0], cfg.Slaves[1] = cfg.Slaves[1], cfg.Slaves[0]
	if err := validateGenesisForTest(cfg); err != nil {
		t.Fatalf("reordering deployment entries changed consensus: %v", err)
	}
	for _, slave := range cfg.Slaves {
		chain := slave.Eth.Genesis.Config
		if !reflect.DeepEqual(chain.QuarkChain.ChainIDs, []*big.Int{big.NewInt(100001), big.NewInt(100002)}) || chain.IsRootCoinbaseChain() != (chain.ChainID.Int64() == 100001) {
			t.Fatal("genesis membership or reward recipient was derived from deployment order")
		}
	}
	ids := cfg.Slaves[0].Eth.Genesis.Config.QuarkChain.ChainIDs
	ids[0], ids[1] = ids[1], ids[0]
	if err := validateGenesisForTest(cfg); err == nil {
		t.Fatal("inconsistent ordered membership accepted")
	}
	ids = cfg.Slaves[1].Eth.Genesis.Config.QuarkChain.ChainIDs
	ids[0], ids[1] = ids[1], ids[0]
	if err := validateGenesisForTest(cfg); err != nil {
		t.Fatalf("consistent genesis-selected order rejected: %v", err)
	}
	if !cfg.Slaves[0].Eth.Genesis.Config.IsRootCoinbaseChain() {
		t.Fatal("root reward chain did not follow genesis membership")
	}
	for _, slave := range cfg.Slaves {
		slave.Eth.Genesis.Config.QuarkChain.ChainIDs = append(slave.Eth.Genesis.Config.QuarkChain.ChainIDs, big.NewInt(100003))
	}
	if err := validateGenesisForTest(cfg); err == nil {
		t.Fatal("undeployed member accepted")
	}
}

func TestInvalidTOML(t *testing.T) {
	for _, test := range []struct {
		name, input string
	}{
		{"wrong case", "networkID = 2"},
		{"network id overflow", "NetworkID = 18446744073709551616"},
		{"negative network id", "NetworkID = -1"},
		{"duplicate key", "NetworkID = 2\nNetworkID = 3"},
		{"obsolete network file", "NetworkFile = \"network.toml\""},
		{"obsolete shared forks", "[ShardForks]\nHomesteadBlock = \"0\""},
		{"root consensus in deployment", "[Root]\nPoSW = {}"},
		{"local typo", "[Master.Node]\nHttpPort = 1234"},
		{"legacy shard nodes", "[[ShardNodes]]\nChainID = \"100001\""},
		{"legacy shard id", "[[Slaves]]\nShardID = 0"},
		{"legacy internal address", "[[Slaves]]\nChainID = \"100001\"\nAddress = \"127.0.0.1:38000\""},
		{"legacy cluster host", "[[Slaves]]\nChainID = \"100001\"\nClusterHost = \"127.0.0.1\""},
		{"invalid public port type", "[Master]\nPublicPort = \"invalid\""},
		{"short master coinbase", "[Master]\nCoinbaseAddress = \"0x12\""},
		{"legacy shard coinbase", "[[Slaves]]\nChainID = \"100001\"\nCoinbaseAddress = \"0x111111111111111111111111111111111111111100000001\""},
		{"invalid shard coinbase", "[[Slaves]]\nChainID = \"100001\"\nCoinbaseAddress = \"0xgg11111111111111111111111111111111111111\""},
		{"nested typo", "[[Slaves]]\nChainID = \"100001\"\n[Slaves.Eth.Miner]\nUnknown = 1"},
		{"invalid sync mode", "[[Slaves]]\nChainID = \"100001\"\n[Slaves.Eth]\nSyncMode = \"invalid\""},
		{"derived role", "[Master]\nRole = \"master\""},
		{"derived root config", "[Master.Root]\nConsensusType = \"POW_ETHASH\""},
		{"duplicate endpoint map", "[Master.ShardEndpoints]\n0 = \"127.0.0.1:38000\""},
		{"invalid chain id text", "[[Slaves]]\nChainID = \"not-a-number\""},
		{"inline genesis", "[[Slaves]]\nChainID = \"100001\"\n[Slaves.Eth.Genesis]\nGasLimit = 10000000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := writeConfig(t, []byte(test.input))
			_, err := LoadCluster(file)
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Fatalf("want an error identifying the file, got %v", err)
			}
			if test.name == "inline genesis" {
				return // Semantic rejection from the custom decoder, not a field lookup error.
			}
			var lineError *toml.LineError
			if !errors.As(err, &lineError) || lineError.Line < 1 {
				t.Fatalf("want a TOML error identifying the line, got %v", err)
			}
		})
	}
	if _, err := LoadCluster(""); err == nil {
		t.Fatal("missing deployment path accepted")
	}
	if _, err := LoadCluster(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("missing local file accepted")
	}
}

func TestClusterExample(t *testing.T) {
	cfg := exampleCluster(t)
	if cfg.Master.GenesisFile != filepath.Join("examples", "genesis", "root.json") {
		t.Fatalf("root genesis path resolved incorrectly: %s", cfg.Master.GenesisFile)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	master, err := cfg.SelectNode(RoleMaster, nil)
	if err != nil {
		t.Fatal(err)
	}
	if master != &cfg.Master || master.Eth != nil || master.Node.P2P.MaxPeers != 25 || len(master.ShardEndpoints) != 2 {
		t.Fatalf("unexpected master configuration: %+v", master)
	}
	if master.CoinbaseAddress != common.HexToAddress("0x1111111111111111111111111111111111111111") {
		t.Fatal("master mining address was not preserved")
	}
	addresses := []common.Address{
		common.HexToAddress("0x2222222222222222222222222222222222222222"),
		common.HexToAddress("0x3333333333333333333333333333333333333333"),
	}
	for i, entry := range cfg.Slaves {
		selected, err := cfg.SelectNode(RoleSlave, entry.ChainID)
		if err != nil {
			t.Fatal(err)
		}
		if selected != &cfg.Slaves[i] || selected.GenesisFile != filepath.Join("examples", "genesis", fmt.Sprintf("shard-%v.json", entry.ChainID)) || selected.ShardEndpoints != nil {
			t.Fatal("process selection did not return the shard's own configuration")
		}
		if selected.CoinbaseAddress != addresses[i] {
			t.Fatal("selected shard has the wrong mining address")
		}
		if master.ShardEndpoints[entry.ChainID.String()] != fmt.Sprintf("127.0.0.1:%d", 38000+i) {
			t.Fatal("master endpoints were not derived from deployment entries")
		}
		if entry.Eth.NetworkId != cfg.NetworkID || entry.Node.HTTPPort != 18545+i {
			t.Fatal("per-shard settings were not preserved")
		}
		if entry.Eth.SyncMode != ethconfig.Defaults.SyncMode || entry.Eth.RPCGasCap != ethconfig.Defaults.RPCGasCap {
			t.Fatal("omitted execution settings did not inherit geth defaults")
		}
	}
	cfg.Slaves[0].Eth.DatabaseCache = 0
	if cfg.Slaves[1].Eth.DatabaseCache != 256 {
		t.Fatal("selected shard settings changed another shard")
	}
}

func TestClusterNetworkID(t *testing.T) {
	for _, test := range []struct {
		name string
		id   uint64
	}{
		{"zero", 0},
		{"above uint32", 1 << 32},
		{"high bit", 1 << 63},
		{"maximum", math.MaxUint64},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := copyExampleCluster(t)
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte("NetworkID = 2"), []byte(fmt.Sprintf("NetworkID = %d", test.id)), 1)
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadCluster(file)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NetworkID != test.id {
				t.Fatalf("decoded NetworkID = %d, want %d", cfg.NetworkID, test.id)
			}
			if err := cfg.LoadGenesis(); err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if test.id == 0 {
				if err == nil || !strings.Contains(err.Error(), "NetworkID must be positive") {
					t.Fatalf("zero network ID was accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, slave := range cfg.Slaves {
				if slave.Eth.NetworkId != test.id {
					t.Fatalf("chain %v: Eth.NetworkId = %d, want %d", slave.ChainID, slave.Eth.NetworkId, test.id)
				}
			}
		})
	}
}

func TestClusterLargeChainID(t *testing.T) {
	id := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	file := copyExampleCluster(t)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`ChainID = "100002"`), []byte(`ChainID = "`+id.String()+`"`), 1)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shard-100001.json", "shard-100002.json"} {
		genesis := filepath.Join(filepath.Dir(file), "genesis", name)
		data, err := os.ReadFile(genesis)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.ReplaceAll(data, []byte("100002"), []byte(id.String()))
		var spec core.Genesis
		if err := json.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		spec.ExtraData = nil
		data, err = json.Marshal(&spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(genesis, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cluster, err := LoadCluster(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	if err := cluster.Validate(); err != nil {
		t.Fatal(err)
	}
	first, second := cluster.Slaves[0].Eth.Genesis.Config, cluster.Slaves[1].Eth.Genesis.Config
	if second.ChainID.Cmp(id) != 0 || first.QuarkChain.ChainIDs[1].Cmp(id) != 0 {
		t.Fatal("large chain ID was not preserved in execution and routing configuration")
	}
	selected, err := cluster.SelectNode(RoleSlave, new(big.Int).Set(id))
	if err != nil || selected != &cluster.Slaves[1] {
		t.Fatalf("large chain ID did not select its Slave: %v", err)
	}
	master, err := cluster.SelectNode(RoleMaster, nil)
	if err != nil || master.ShardEndpoints[id.String()] != "127.0.0.1:38001" {
		t.Fatalf("large chain ID did not key the Master endpoint: %v", err)
	}
	first.QuarkChain.ChainIDs[1].SetInt64(7)
	if second.QuarkChain.ChainIDs[1].Cmp(id) != 0 || second.ChainID.Cmp(id) != 0 {
		t.Fatal("large genesis chain ID aliases another shard or its identity")
	}
}

func TestNodeDefaults(t *testing.T) {
	master := DefaultNodeConfig(nil)
	id := big.NewInt(100001)
	shard := DefaultNodeConfig(id)
	id.SetInt64(7)
	if shard.ChainID.Cmp(big.NewInt(100001)) != 0 {
		t.Fatal("default Slave identity aliases its input")
	}
	if master.Eth != nil || master.Node.P2P.MaxPeers != node.DefaultConfig.P2P.MaxPeers || shard.Eth.DatabaseCache != ethconfig.Defaults.DatabaseCache {
		t.Fatal("geth defaults were not preserved")
	}
	for _, cfg := range []NodeConfig{master, shard} {
		if cfg.Node.HTTPHost != "" || cfg.Node.WSHost != "" {
			t.Fatal("unexpected endpoint defaults")
		}
		if len(cfg.Node.P2P.BootstrapNodes) != 0 || len(cfg.Node.P2P.BootstrapNodesV5) != 0 {
			t.Fatal("an implicit network was selected")
		}
	}
	if shard.Eth.NetworkId != 0 || shard.Node.P2P.ListenAddr != "" || shard.Node.P2P.MaxPeers != 0 || !shard.Node.P2P.NoDiscovery || !shard.Node.P2P.NoDial {
		t.Fatal("slave external P2P was not disabled")
	}
	if (master.Node.DataDir != "" && master.Node.DataDir == shard.Node.DataDir) || master.Node.Name == shard.Node.Name {
		t.Fatal("process defaults share a data directory or instance name")
	}
	otherID := big.NewInt(100002)
	for _, ephemeral := range []bool{false, true} {
		endpoints := make(map[string]bool)
		for _, cfg := range []NodeConfig{master, shard, DefaultNodeConfig(otherID)} {
			if ephemeral {
				cfg.Node.DataDir = ""
			}
			endpoint := cfg.Node.IPCEndpoint()
			if endpoint == "" || endpoints[endpoint] {
				t.Fatalf("default IPC endpoints must be nonempty and distinct (ephemeral=%t): %q", ephemeral, endpoint)
			}
			endpoints[endpoint] = true
		}
	}
}

func TestLargeChainIDDefaultIPC(t *testing.T) {
	savedDataDir := node.DefaultConfig.DataDir
	node.DefaultConfig.DataDir = "data"
	t.Cleanup(func() { node.DefaultConfig.DataDir = savedDataDir })

	small := DefaultNodeConfig(big.NewInt(100001))
	if small.Node.DataDir != filepath.Join("data", "qkc", "shard-100001") || small.Node.IPCPath != "qkc-shard-100001.ipc" {
		t.Fatalf("small chain ID defaults changed: datadir=%q ipc=%q", small.Node.DataDir, small.Node.IPCPath)
	}
	if runtime.GOOS == "windows" {
		return // Windows named pipes do not have the Unix socket path limit.
	}
	maxID := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	maxNode := DefaultNodeConfig(maxID)
	again := DefaultNodeConfig(new(big.Int).Set(maxID))
	other := DefaultNodeConfig(new(big.Int).Sub(maxID, big.NewInt(1)))
	if endpoint := maxNode.Node.IPCEndpoint(); len(endpoint) > 103 {
		t.Fatalf("maximum chain ID default IPC endpoint is too long for macOS: %d bytes: %q", len(endpoint), endpoint)
	}
	if maxNode.Node.IPCEndpoint() != again.Node.IPCEndpoint() || maxNode.Node.IPCEndpoint() == other.Node.IPCEndpoint() || maxNode.Node.DataDir == other.Node.DataDir {
		t.Fatal("large chain ID defaults must be stable and distinct")
	}

	cluster := exampleCluster(t)
	cluster.Slaves[1].ChainID = new(big.Int).Set(maxID)
	cluster.Slaves[1].Node.DataDir = maxNode.Node.DataDir
	cluster.Slaves[1].Node.IPCPath = maxNode.Node.IPCPath
	cluster.Slaves[1].Eth.Genesis.Config.ChainID = new(big.Int).Set(maxID)
	for i := range cluster.Slaves {
		cluster.Slaves[i].Eth.Genesis.Config.QuarkChain.ChainIDs[1] = new(big.Int).Set(maxID)
	}
	if err := cluster.Validate(); err != nil {
		t.Fatalf("maximum chain ID deployment should be valid with default IPC path: %v", err)
	}
}

func TestClusterPublicEndpoint(t *testing.T) {
	for _, test := range []struct {
		name, input, host string
		port              int
		master            bool
	}{
		{"master", "[Master]\nPublicHost = \"127.0.0.1\"\nPublicPort = 18544", "127.0.0.1", 18544, true},
		{"shard", "[[Slaves]]\nChainID = \"100001\"\nPublicHost = \"127.0.0.1\"\nPublicPort = 18545", "127.0.0.1", 18545, false},
		{"inherited public port", "[[Slaves]]\nChainID = \"100001\"\nPublicHost = \"127.0.0.1\"", "127.0.0.1", node.DefaultHTTPPort, false},
		{"public port leaves HTTP disabled", "[[Slaves]]\nChainID = \"100001\"\nPublicPort = 18545", "", 18545, false},
		{"native fallback", "[[Slaves]]\nChainID = \"100001\"\n[Slaves.Node]\nHTTPHost = \"127.0.0.2\"\nHTTPPort = 18547", "127.0.0.2", 18547, false},
		{"public override", "[[Slaves]]\nChainID = \"100001\"\nPublicHost = \"127.0.0.1\"\nPublicPort = 18545\n[Slaves.Node]\nHTTPHost = \"127.0.0.2\"\nHTTPPort = 18547", "127.0.0.1", 18545, false},
		{"explicit empty and zero", "[[Slaves]]\nChainID = \"100001\"\nPublicHost = \"\"\nPublicPort = 0\n[Slaves.Node]\nHTTPHost = \"127.0.0.2\"\nHTTPPort = 18547", "", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := LoadCluster(writeConfig(t, []byte(test.input)))
			if err != nil {
				t.Fatal(err)
			}
			selected := &cfg.Master
			if !test.master {
				selected = &cfg.Slaves[0]
			}
			if selected.Node.HTTPHost != test.host || selected.Node.HTTPPort != test.port {
				t.Fatalf("effective HTTP endpoint = %q:%d, want %q:%d", selected.Node.HTTPHost, selected.Node.HTTPPort, test.host, test.port)
			}
		})
	}
}

func TestClusterDefaultOverrides(t *testing.T) {
	cfg, err := LoadCluster(writeConfig(t, []byte(`[Master.Node.P2P]
MaxPeers = 0
DiscoveryV4 = false
[[Slaves]]
ChainID = "100001"
PrivateHost = "127.0.0.1"
PrivatePort = 38000
[Slaves.Eth]
DatabaseCache = 0
[Slaves.Eth.Miner]
GasPrice = "123"
[Slaves.Eth.GPO]
MaxPrice = "456"
IgnorePrice = "7"
[[Slaves]]
ChainID = "100002"
PrivateHost = "127.0.0.1"
PrivatePort = 38001
`)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Master.Node.P2P.MaxPeers != 0 || cfg.Master.Node.P2P.DiscoveryV4 || cfg.Slaves[0].Eth.DatabaseCache != 0 {
		t.Fatal("explicit zero or false was replaced by defaults")
	}
	first, second := cfg.Slaves[0].Eth, cfg.Slaves[1].Eth
	if first.Miner.GasPrice.Int64() != 123 || first.GPO.MaxPrice.Int64() != 456 || first.GPO.IgnorePrice.Int64() != 7 {
		t.Fatal("explicit big integer overrides were not applied")
	}
	if second.DatabaseCache != ethconfig.Defaults.DatabaseCache || second.Miner.GasPrice.Cmp(ethconfig.Defaults.Miner.GasPrice) != 0 || second.GPO.MaxPrice.Cmp(ethconfig.Defaults.GPO.MaxPrice) != 0 || second.GPO.IgnorePrice.Cmp(ethconfig.Defaults.GPO.IgnorePrice) != 0 {
		t.Fatal("shard overrides affected another shard or its defaults")
	}
	second.Miner.GasPrice.SetInt64(999)
	second.GPO.MaxPrice.SetInt64(999)
	second.GPO.IgnorePrice.SetInt64(999)
	if ethconfig.Defaults.Miner.GasPrice.Int64() == 999 || ethconfig.Defaults.GPO.MaxPrice.Int64() == 999 || ethconfig.Defaults.GPO.IgnorePrice.Int64() == 999 {
		t.Fatal("mutable geth defaults are shared with a shard")
	}
	// Omitting the entire Master table still supplies its defaults.
	cfg, err = LoadCluster(writeConfig(t, []byte("[[Slaves]]\nChainID = \"100001\"\nPrivateHost = \"localhost\"\nPrivatePort = 38000\n")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Master.Role != RoleMaster || cfg.Master.Node.P2P.MaxPeers != node.DefaultConfig.P2P.MaxPeers {
		t.Fatal("omitted master lost its defaults")
	}
}

func TestClusterNestedDefaults(t *testing.T) {
	// Use a true default to distinguish an explicit false from an omitted field.
	savedNoLocals := ethconfig.Defaults.TxPool.NoLocals
	ethconfig.Defaults.TxPool.NoLocals = true
	t.Cleanup(func() { ethconfig.Defaults.TxPool.NoLocals = savedNoLocals })
	for _, test := range []struct {
		name   string
		input  string
		change func(*ethconfig.Config)
	}{
		{"miner gas price", "[Slaves.Eth.Miner]\nGasPrice = \"123\"", func(c *ethconfig.Config) {
			c.Miner.GasPrice = big.NewInt(123)
		}},
		{"miner zeros", "[Slaves.Eth.Miner]\nGasCeil = 0\nRecommit = 0\nGasPrice = \"0\"", func(c *ethconfig.Config) {
			c.Miner.GasCeil, c.Miner.Recommit, c.Miner.GasPrice = 0, 0, new(big.Int)
		}},
		{"GPO price", "[Slaves.Eth.GPO]\nMaxPrice = \"456\"", func(c *ethconfig.Config) {
			c.GPO.MaxPrice = big.NewInt(456)
		}},
		{"GPO zeros", "[Slaves.Eth.GPO]\nBlocks = 0\nMaxPrice = \"0\"", func(c *ethconfig.Config) {
			c.GPO.Blocks, c.GPO.MaxPrice = 0, new(big.Int)
		}},
		{"txpool price bump", "[Slaves.Eth.TxPool]\nPriceBump = 25", func(c *ethconfig.Config) {
			c.TxPool.PriceBump = 25
		}},
		{"txpool zero and false", "[Slaves.Eth.TxPool]\nPriceLimit = 0\nNoLocals = false", func(c *ethconfig.Config) {
			c.TxPool.PriceLimit, c.TxPool.NoLocals = 0, false
		}},
		{"blobpool capacity", "[Slaves.Eth.BlobPool]\nDatacap = 123", func(c *ethconfig.Config) {
			c.BlobPool.Datacap = 123
		}},
		{"blobpool zero", "[Slaves.Eth.BlobPool]\nDatacap = 0", func(c *ethconfig.Config) {
			c.BlobPool.Datacap = 0
		}},
		{"empty tables", "[Slaves.Eth.Miner]\n[Slaves.Eth.GPO]\n[Slaves.Eth.TxPool]\n[Slaves.Eth.BlobPool]", func(*ethconfig.Config) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := "[[Slaves]]\nChainID = \"100001\"\n" + test.input + "\n[[Slaves]]\nChainID = \"100002\"\n"
			cfg, err := LoadCluster(writeConfig(t, []byte(input)))
			if err != nil {
				t.Fatal(err)
			}
			want := ethconfig.Defaults
			test.change(&want)
			got := cfg.Slaves[0].Eth
			for _, prices := range [][2]*big.Int{
				{got.Miner.GasPrice, want.Miner.GasPrice},
				{got.GPO.MaxPrice, want.GPO.MaxPrice},
				{got.GPO.IgnorePrice, want.GPO.IgnorePrice},
			} {
				if prices[0] == nil || prices[0].Cmp(prices[1]) != 0 {
					t.Fatalf("gas price = %v, want %v", prices[0], prices[1])
				}
			}
			// Compare big integers numerically, independently of their representation.
			want.Miner.GasPrice, want.GPO.MaxPrice, want.GPO.IgnorePrice = got.Miner.GasPrice, got.GPO.MaxPrice, got.GPO.IgnorePrice
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("nested override lost defaults or explicit values:\nhave %+v\nwant %+v", got, &want)
			}
			if !reflect.DeepEqual(cfg.Slaves[1].Eth, &ethconfig.Defaults) {
				t.Fatal("nested overrides affected another shard")
			}
		})
	}
}

func TestProcessSelection(t *testing.T) {
	zero, unknown := new(big.Int), big.NewInt(100003)
	for _, test := range []struct {
		role string
		id   *big.Int
		want string
	}{
		{"", nil, "set --qkc.role"},
		{"shard", nil, "set --qkc.role"},
		{RoleMaster, zero, "master cannot select"},
		{RoleSlave, nil, "requires a chain ID"},
		{RoleSlave, unknown, "not in the deployment"},
		{RoleSlave, zero, "not in the deployment"},
	} {
		if _, err := exampleCluster(t).SelectNode(test.role, test.id); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("have %v, want error containing %q", err, test.want)
		}
	}
}

func TestClusterValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ClusterConfig)
		want   string
	}{
		{"no shards", func(c *ClusterConfig) { c.Slaves = nil }, "at least one shard"},
		{"missing id", func(c *ClusterConfig) { c.Slaves[0].ChainID = nil }, "ChainID"},
		{"duplicate shard", func(c *ClusterConfig) { c.Slaves[1].ChainID = big.NewInt(100001) }, "duplicate deployed chain"},
		{"zero shard", func(c *ClusterConfig) { c.Slaves[1].ChainID = new(big.Int) }, "positive uint256"},
		{"negative shard", func(c *ClusterConfig) { c.Slaves[1].ChainID = big.NewInt(-1) }, "positive uint256"},
		{"overflow shard", func(c *ClusterConfig) { c.Slaves[1].ChainID = new(big.Int).Lsh(big.NewInt(1), 256) }, "positive uint256"},
		{"duplicate cluster endpoint", func(c *ClusterConfig) {
			c.Slaves[1].PrivateHost = c.Slaves[0].PrivateHost
			c.Slaves[1].PrivatePort = c.Slaves[0].PrivatePort
		}, "TCP listeners conflict"},
		{"equivalent IPv6 addresses", func(c *ClusterConfig) {
			c.Slaves[0].PrivateHost = "::1"
			c.Slaves[1].PrivateHost = "0:0:0:0:0:0:0:1"
			c.Slaves[1].PrivatePort = c.Slaves[0].PrivatePort
		}, "TCP listeners conflict"},
		{"IPv4 mapped address", func(c *ClusterConfig) {
			c.Slaves[1].PrivateHost = "::ffff:127.0.0.1"
			c.Slaves[1].PrivatePort = c.Slaves[0].PrivatePort
		}, "TCP listeners conflict"},
		{"shared datadir", func(c *ClusterConfig) { c.Slaves[1].Node.DataDir = c.Master.Node.DataDir }, "data directory is shared"},
		{"master chain id", func(c *ClusterConfig) { c.Master.ChainID = new(big.Int) }, "Master must not specify ChainID"},
		{"master execution", func(c *ClusterConfig) { c.Master.Eth = &ethconfig.Config{} }, "master cannot configure Eth"},
		{"master cluster host", func(c *ClusterConfig) { c.Master.PrivateHost = "localhost" }, "master cannot configure Eth"},
		{"master cluster port", func(c *ClusterConfig) { c.Master.PrivatePort = 38002 }, "master cannot configure Eth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleCluster(t)
			test.change(cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
	cfg := exampleCluster(t)
	cfg.Master.Node.DataDir, cfg.Slaves[0].Node.DataDir, cfg.Slaves[1].Node.DataDir = "", "", ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ephemeral data directories should be supported: %v", err)
	}
}

func TestClusterTCPEndpoints(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*ClusterConfig)
		conflict bool
	}{
		{"duplicate HTTP", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, true},
		{"master and shard HTTP", func(c *ClusterConfig) {
			c.Master.Node.HTTPHost = c.Slaves[0].Node.HTTPHost
			c.Master.Node.HTTPPort = c.Slaves[0].Node.HTTPPort
		}, true},
		{"HTTP and WS across processes", func(c *ClusterConfig) {
			c.Slaves[1].Node.WSHost = c.Slaves[0].Node.HTTPHost
			c.Slaves[1].Node.WSPort = c.Slaves[0].Node.HTTPPort
		}, true},
		{"duplicate WS", func(c *ClusterConfig) {
			c.Slaves[0].Node.WSHost, c.Slaves[1].Node.WSHost = "localhost", "localhost"
		}, true},
		{"equivalent IPv6", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost = "::1"
			c.Slaves[1].Node.HTTPHost = "0:0:0:0:0:0:0:1"
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, true},
		{"hostname case", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost, c.Slaves[1].Node.HTTPHost = "LOCALHOST", "localhost"
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, true},
		{"wildcard first", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost = "0.0.0.0"
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, true},
		{"wildcard second", func(c *ClusterConfig) {
			c.Slaves[1].Node.HTTPHost = "::"
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, true},
		{"HTTP and own internal listener", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPPort = 38000
		}, true},
		{"HTTP and later internal listener", func(c *ClusterConfig) {
			c.Master.Node.HTTPHost, c.Master.Node.HTTPPort = "127.0.0.1", 38001
		}, true},
		{"WS and earlier internal listener", func(c *ClusterConfig) {
			c.Slaves[1].Node.WSHost, c.Slaves[1].Node.WSPort = "127.0.0.1", 38000
		}, true},
		{"default WS host and internal listener", func(c *ClusterConfig) {
			c.Slaves[0].Node.WSHost = node.DefaultWSHost
			c.Slaves[0].Node.WSPort = c.Slaves[0].PrivatePort
		}, true},
		{"internal hostname and HTTP IP", func(c *ClusterConfig) {
			c.Slaves[0].PrivateHost = "localhost"
			c.Slaves[0].Node.HTTPPort = c.Slaves[0].PrivatePort
		}, true},
		{"HTTP hostname and later internal IP", func(c *ClusterConfig) {
			c.Master.Node.HTTPHost, c.Master.Node.HTTPPort = "LOCALHOST", 38000
		}, true},
		{"P2P hostname and HTTP IP", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = "localhost:18545"
		}, true},
		{"default WS host and mapped IPv4", func(c *ClusterConfig) {
			c.Slaves[0].PrivateHost = "::ffff:127.0.0.1"
			c.Slaves[0].Node.WSHost = node.DefaultWSHost
			c.Slaves[0].Node.WSPort = c.Slaves[0].PrivatePort
		}, true},
		{"P2P and shard HTTP", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPPort = 30303
		}, true},
		{"P2P and master HTTP", func(c *ClusterConfig) {
			c.Master.Node.HTTPHost, c.Master.Node.HTTPPort = "127.0.0.1", 30303
		}, true},
		{"P2P and internal listener", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = "127.0.0.1:38000"
		}, true},
		{"P2P and wildcard WS", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = "127.0.0.1:30303"
			c.Slaves[0].Node.WSHost, c.Slaves[0].Node.WSPort = "0.0.0.0", 30303
		}, true},
		{"internal listener and mapped IPv4 HTTP", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost, c.Slaves[0].Node.HTTPPort = "::ffff:127.0.0.1", 38000
		}, true},
		{"shared WS listener and internal listener", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost = "127.0.0.2"
			c.Slaves[0].Node.WSHost = "127.0.0.1"
			c.Slaves[0].Node.HTTPPort, c.Slaves[0].Node.WSPort = 38000, 38000
		}, true},
		{"dynamic ports", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPPort, c.Slaves[1].Node.HTTPPort = 0, 0
		}, false},
		{"disabled HTTP", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost = ""
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, false},
		{"same port on different hosts", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost = "127.0.0.2"
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
		}, false},
		{"HTTP and WS in one process", func(c *ClusterConfig) {
			c.Slaves[0].Node.WSHost = c.Slaves[0].Node.HTTPHost
			c.Slaves[0].Node.WSPort = c.Slaves[0].Node.HTTPPort
		}, false},
		{"shared listener uses WS host", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPPort = c.Slaves[1].Node.HTTPPort
			c.Slaves[0].Node.WSHost = "127.0.0.2"
			c.Slaves[0].Node.WSPort = c.Slaves[0].Node.HTTPPort
		}, false},
		{"shared WS listener avoids internal listener", func(c *ClusterConfig) {
			c.Slaves[0].Node.WSHost = "127.0.0.2"
			c.Slaves[0].Node.HTTPPort, c.Slaves[0].Node.WSPort = 38000, 38000
		}, false},
		{"internal and HTTP on different hosts", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost, c.Slaves[0].Node.HTTPPort = "127.0.0.2", 38000
		}, false},
		{"default WS host and distinct loopback IP", func(c *ClusterConfig) {
			c.Slaves[0].PrivateHost = "127.0.0.2"
			c.Slaves[0].Node.WSHost = node.DefaultWSHost
			c.Slaves[0].Node.WSPort = c.Slaves[0].PrivatePort
		}, false},
		{"shared WS listener ignores HTTP hostname", func(c *ClusterConfig) {
			c.Slaves[0].Node.HTTPHost, c.Slaves[0].Node.HTTPPort = "localhost", 38000
			c.Slaves[0].Node.WSHost, c.Slaves[0].Node.WSPort = "127.0.0.2", 38000
		}, false},
		{"P2P and HTTP on different hosts", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = "127.0.0.2:30303"
			c.Slaves[0].Node.HTTPPort = 30303
		}, false},
		{"disabled P2P listener", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = ""
			c.Slaves[0].Node.HTTPPort = 30303
		}, false},
		{"dynamic P2P port", func(c *ClusterConfig) {
			c.Master.Node.P2P.ListenAddr = ":0"
		}, false},
		{"UDP discovery and internal TCP", func(c *ClusterConfig) {
			c.Master.Node.P2P.DiscAddr = ":38000"
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleCluster(t)
			test.change(cfg)
			err := cfg.Validate()
			if test.conflict {
				if err == nil || !strings.Contains(err.Error(), "TCP listeners conflict") {
					t.Fatalf("want TCP listener conflict, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClusterIPCEndpoints(t *testing.T) {
	absolute, err := filepath.Abs("ipc/shared.ipc")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		change   func(*ClusterConfig)
		conflict bool
	}{
		{"shared explicit path", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "ipc/shared.ipc", "ipc/shared.ipc"
		}, true},
		{"normalized paths", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "ipc/shared.ipc", "ipc/./shared.ipc"
		}, true},
		{"resolved bare filename", func(c *ClusterConfig) {
			c.Slaves[0].Node.IPCPath = c.Master.Node.IPCEndpoint()
		}, true},
		{"same name with empty datadirs", func(c *ClusterConfig) {
			c.Master.Node.DataDir, c.Slaves[0].Node.DataDir = "", ""
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "shared.ipc", "shared.ipc"
		}, true},
		{"same name with distinct datadirs", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "shared.ipc", "shared.ipc"
		}, runtime.GOOS == "windows"},
		{"absolute and relative paths", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = absolute, "ipc/shared.ipc"
		}, runtime.GOOS != "windows"},
		{"named pipe case", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "shared.ipc", "SHARED.ipc"
		}, runtime.GOOS == "windows"},
		{"disabled IPC", func(c *ClusterConfig) {
			c.Master.Node.IPCPath, c.Slaves[0].Node.IPCPath = "", ""
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleCluster(t)
			test.change(cfg)
			err := cfg.Validate()
			if test.conflict {
				if err == nil || !strings.Contains(err.Error(), "IPC endpoint is shared") {
					t.Fatalf("want IPC endpoint conflict, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClusterRejectsOverlongIPC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows named pipes do not have the Unix socket path limit")
	}
	cluster := exampleCluster(t)
	cluster.Slaves[0].Node.IPCPath = strings.Repeat("i", 104) + ".ipc"
	if err := cluster.Slaves[0].Validate(cluster.NetworkID); err == nil || !strings.Contains(err.Error(), "IPC endpoint") {
		t.Fatalf("overlong IPC endpoint accepted by node validation: %v", err)
	}
	if err := cluster.Validate(); err == nil || !strings.Contains(err.Error(), "IPC endpoint") {
		t.Fatalf("overlong IPC endpoint accepted: %v", err)
	}
}

func TestNodeValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		master bool
		change func(*NodeConfig)
		want   string
	}{
		{"network mismatch", false, func(c *NodeConfig) { c.Eth.NetworkId = 100001 }, "conflicts with NetworkID"},
		{"local fork", false, func(c *NodeConfig) { c.Eth.OverrideOsaka = new(uint64) }, "local fork overrides"},
		{"database engine", true, func(c *NodeConfig) { c.Node.DBEngine = "unknown" }, "Node.DBEngine"},
		{"state scheme", false, func(c *NodeConfig) { c.Eth.StateScheme = "unknown" }, "Eth.StateScheme"},
		{"negative peers", true, func(c *NodeConfig) { c.Node.P2P.MaxPeers = -1 }, "MaxPeers"},
		{"negative cache", false, func(c *NodeConfig) { c.Eth.DatabaseCache = -1 }, "DatabaseCache"},
		{"public port", false, func(c *NodeConfig) { c.Node.HTTPPort = 65536 }, "PublicPort"},
		{"ws port", false, func(c *NodeConfig) { c.Node.WSPort = -1 }, "WSPort"},
		{"auth port", true, func(c *NodeConfig) { c.Node.AuthPort = -1 }, "AuthPort"},
		{"listen address", true, func(c *NodeConfig) { c.Node.P2P.ListenAddr = "invalid" }, "ListenAddr"},
		{"listen port", true, func(c *NodeConfig) { c.Node.P2P.ListenAddr = ":65536" }, "invalid port"},
		{"shard external peers", false, func(c *NodeConfig) { c.Node.P2P.MaxPeers = 1 }, "external P2P must be disabled"},
		{"shard external listen", false, func(c *NodeConfig) { c.Node.P2P.ListenAddr = ":30304" }, "external P2P must be disabled"},
		{"shard external discovery", false, func(c *NodeConfig) { c.Node.P2P.NoDiscovery = false }, "external P2P must be disabled"},
		{"missing cluster host", false, func(c *NodeConfig) { c.PrivateHost = "" }, "PrivateHost must be a concrete host"},
		{"wildcard host", false, func(c *NodeConfig) { c.PrivateHost = "0.0.0.0" }, "concrete host"},
		{"wildcard ipv6", false, func(c *NodeConfig) { c.PrivateHost = "0:0:0:0:0:0:0:0" }, "concrete host"},
		{"host includes port", false, func(c *NodeConfig) { c.PrivateHost = "localhost:38000" }, "without brackets or a port"},
		{"bracketed ipv6", false, func(c *NodeConfig) { c.PrivateHost = "[::1]" }, "without brackets or a port"},
		{"zero cluster port", false, func(c *NodeConfig) { c.PrivatePort = 0 }, "PrivatePort must be between"},
		{"negative cluster port", false, func(c *NodeConfig) { c.PrivatePort = -1 }, "PrivatePort must be between"},
		{"overflow cluster port", false, func(c *NodeConfig) { c.PrivatePort = 65536 }, "PrivatePort must be between"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster := exampleCluster(t)
			cfg := &cluster.Slaves[0]
			if test.master {
				cfg = &cluster.Master
			}
			test.change(cfg)
			if err := cfg.Validate(cluster.NetworkID); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
	cluster := exampleCluster(t)
	cfg := &cluster.Master
	cfg.Node.HTTPPort, cfg.Node.WSPort, cfg.Node.AuthPort = 0, 0, 0
	if err := cfg.Validate(cluster.NetworkID); err != nil {
		t.Fatalf("port zero must be valid: %v", err)
	}
	cluster = exampleCluster(t)
	cluster.Slaves[0].PrivateHost = "::1"
	if err := cluster.Validate(); err != nil {
		t.Fatalf("IPv6 internal endpoints must be valid: %v", err)
	}
	master, err := cluster.SelectNode(RoleMaster, nil)
	if err != nil || master.ShardEndpoints["100001"] != "[::1]:38000" {
		t.Fatalf("IPv6 cluster host was not joined correctly: %v", err)
	}
}
