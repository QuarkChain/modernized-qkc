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
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/naoina/toml"
)

func exampleNetwork(t *testing.T) *NetworkConfig {
	t.Helper()
	cfg, err := LoadNetwork("examples/network.toml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeConfig(t *testing.T, content []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNetworkExample(t *testing.T) {
	cfg := exampleNetwork(t)
	if cfg.NetworkID != 2 || len(cfg.Shards) != 2 {
		t.Fatalf("unexpected network: %+v", cfg)
	}
	if !cfg.Root.PoSW.StakingEnabled || cfg.Shards[0].QuarkChain.PoSW.StakingEnabled || !cfg.Shards[1].QuarkChain.PoSW.StakingEnabled {
		t.Fatal("root and shard PoSW configurations are not independent")
	}
	if cfg.Root.PoSW.StakePerBlock.String() != "1000000000000000000000000000000" {
		t.Fatal("large stake was not preserved")
	}
	if cfg.Root.PoSW.DecayFactor.Cmp(big.NewRat(22, 25)) != 0 || cfg.Shards[0].QuarkChain.PoSW.RewardTaxRate.Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatal("consensus ratios were not parsed exactly")
	}
	for _, shard := range cfg.Shards {
		if !slices.Equal(shard.QuarkChain.ShardIDs, []uint32{0, 1}) {
			t.Fatalf("unexpected shard topology: %v", shard.QuarkChain.ShardIDs)
		}
	}
	// Equal numeric values do not conflate the network and EVM identifiers.
	cfg.Shards[0].ChainID.SetUint64(uint64(cfg.NetworkID))
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*NetworkConfig)
		want   string
	}{
		{"zero network", func(c *NetworkConfig) { c.NetworkID = 0 }, "NetworkID must be positive"},
		{"no shards", func(c *NetworkConfig) { c.Shards = nil }, "at least one shard"},
		{"no quarkchain", func(c *NetworkConfig) { c.Shards[0].QuarkChain = nil }, "QuarkChain configuration is required"},
		{"duplicate shard", func(c *NetworkConfig) { c.Shards[1].QuarkChain.ShardID = 0 }, "duplicate shard ID"},
		{"reserved shard", func(c *NetworkConfig) { c.Shards[1].QuarkChain.ShardID = math.MaxUint32 }, "reserved for the root"},
		{"missing shard zero", func(c *NetworkConfig) { c.Shards = c.Shards[1:] }, "include shard 0"},
		{"missing chain id", func(c *NetworkConfig) { c.Shards[0].ChainID = nil }, "positive uint256"},
		{"zero chain id", func(c *NetworkConfig) { c.Shards[0].ChainID = new(big.Int) }, "positive uint256"},
		{"negative chain id", func(c *NetworkConfig) { c.Shards[0].ChainID = big.NewInt(-1) }, "positive uint256"},
		{"overflow chain id", func(c *NetworkConfig) { c.Shards[0].ChainID = new(big.Int).Lsh(big.NewInt(1), 256) }, "positive uint256"},
		{"duplicate chain id", func(c *NetworkConfig) { c.Shards[1].ChainID = c.Shards[0].ChainID }, "duplicate EVM ChainID"},
		{"missing peer", func(c *NetworkConfig) { c.Shards[0].QuarkChain.ShardIDs = []uint32{0} }, "every network shard"},
		{"unknown peer", func(c *NetworkConfig) { c.Shards[0].QuarkChain.ShardIDs = []uint32{0, 2} }, "unknown or duplicate"},
		{"duplicate peer", func(c *NetworkConfig) { c.Shards[0].QuarkChain.ShardIDs = []uint32{0, 0} }, "unknown or duplicate"},
		{"missing root posw", func(c *NetworkConfig) { c.Root.PoSW = nil }, "Root: posw:"},
		{"missing shard posw", func(c *NetworkConfig) { c.Shards[0].QuarkChain.PoSW = nil }, "shard 0: posw:"},
		{"invalid ordinary pow", func(c *NetworkConfig) { c.Shards[0].QuarkChain.PoSW.MinimumDifficulty = nil }, "shard 0: posw:"},
		{"invalid root posw", func(c *NetworkConfig) { c.Root.PoSW.WindowSize = 0 }, "Root: posw:"},
		{"invalid shard posw", func(c *NetworkConfig) { c.Shards[1].QuarkChain.PoSW.DiffDivider = 0 }, "shard 1: posw:"},
		{"negative stake", func(c *NetworkConfig) { c.Root.PoSW.StakePerBlock.SetInt64(-1) }, "Root: posw:"},
		{"overflow stake", func(c *NetworkConfig) { c.Root.PoSW.StakePerBlock.Lsh(big.NewInt(1), 256) }, "Root: posw:"},
		{"invalid fork order", func(c *NetworkConfig) { c.Shards[0].HomesteadBlock.SetInt64(1) }, "unsupported fork ordering"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := exampleNetwork(t)
			test.change(cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestNetworkDefaults(t *testing.T) {
	const pow = `AdjustmentCutoff = 7
AdjustmentFactor = 512
MinimumDifficulty = "1"
CoinbaseAmount = "0"
RewardTaxRate = "0"
EpochInterval = 3153600
DecayFactor = "0.88"
`
	cfg, err := LoadNetwork(writeConfig(t, []byte(`NetworkID = 9
[Root.PoSW]
`+pow+`
[[Shards]]
ChainID = "10"
[Shards.QuarkChain]
ShardID = 1
[Shards.QuarkChain.PoSW]
`+pow+`
[[Shards]]
ChainID = "11"
[Shards.QuarkChain]
ShardID = 0
[Shards.QuarkChain.PoSW]
`+pow)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root.PoSW.StakingEnabled || cfg.Shards[0].QuarkChain.PoSW.StakingEnabled || cfg.Shards[1].QuarkChain.PoSW.StakingEnabled {
		t.Fatal("staking should be disabled by omission")
	}
	if !slices.Equal(cfg.Shards[0].QuarkChain.ShardIDs, []uint32{0, 1}) {
		t.Fatal("derived shard IDs must be sorted")
	}
	cfg.Shards[0].QuarkChain.ShardIDs[0] = 7
	if cfg.Shards[1].QuarkChain.ShardIDs[0] != 0 {
		t.Fatal("derived topology slices alias each other")
	}
}

func TestInvalidTOML(t *testing.T) {
	for _, test := range []struct {
		name    string
		network bool
		input   string
	}{
		{"wrong case", true, "networkID = 2"},
		{"network id overflow", true, "NetworkID = 4294967296"},
		{"negative network id", true, "NetworkID = -1"},
		{"duplicate key", true, "NetworkID = 2\nNetworkID = 3"},
		{"local network field", true, "[Node]\nDataDir = \"data\""},
		{"network local field", false, "[Root]\nPoSW = {}"},
		{"local typo", false, "[Node]\nHttpPort = 1234"},
		{"nested typo", false, "[Eth.Miner]\nUnknown = 1"},
		{"invalid sync mode", false, "[Eth]\nSyncMode = \"invalid\""},
		{"obsolete posw flag", true, "[Root.PoSW]\nEnabled = true"},
		{"invalid stake", true, "[Root.PoSW]\nStakePerBlock = \"invalid\""},
		{"invalid ratio", true, "[Root.PoSW]\nDecayFactor = \"invalid\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := writeConfig(t, []byte(test.input))
			var err error
			if test.network {
				_, err = LoadNetwork(file)
			} else {
				_, err = LoadNode(file)
			}
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Fatalf("want an error identifying the file, got %v", err)
			}
			var lineError *toml.LineError
			if !errors.As(err, &lineError) || lineError.Line < 1 {
				t.Fatalf("want a TOML error identifying the line, got %v", err)
			}
		})
	}
	if _, err := LoadNetwork(""); err == nil {
		t.Fatal("missing network file accepted")
	}
	if _, err := LoadNode(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("missing local file accepted")
	}
}

func TestNodeDefaultsAndExample(t *testing.T) {
	defaults, err := LoadNode("")
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Eth.DatabaseCache != ethconfig.Defaults.DatabaseCache || defaults.Node.P2P.MaxPeers != node.DefaultConfig.P2P.MaxPeers {
		t.Fatal("geth defaults were not preserved")
	}
	if defaults.Node.IPCPath != "qkc.ipc" || defaults.Node.HTTPHost != "" || defaults.Node.WSHost != "" {
		t.Fatal("unexpected endpoint defaults")
	}
	if len(defaults.Node.P2P.BootstrapNodes) != 0 || len(defaults.Node.P2P.BootstrapNodesV5) != 0 || defaults.Eth.NetworkId != 0 {
		t.Fatal("an implicit network was selected")
	}
	cfg, err := LoadNode("examples/node.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkFile != filepath.Join("examples", "network.toml") || cfg.Eth.DatabaseCache != 512 || cfg.Node.HTTPHost != "127.0.0.1" {
		t.Fatal("local example did not override defaults")
	}
	network := exampleNetwork(t)
	if err := cfg.Validate(network); err != nil {
		t.Fatal(err)
	}
	if cfg.Eth.NetworkId != uint64(network.NetworkID) {
		t.Fatal("network ID was not resolved from the network definition")
	}
	if defaults.Eth.DatabaseCache != ethconfig.Defaults.DatabaseCache {
		t.Fatal("loading mutated defaults")
	}
	zero, err := LoadNode(writeConfig(t, []byte("[Eth]\nDatabaseCache = 0\n[Node.P2P]\nMaxPeers = 0\nDiscoveryV4 = false\n")))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Eth.DatabaseCache != 0 || zero.Node.P2P.MaxPeers != 0 || zero.Node.P2P.DiscoveryV4 {
		t.Fatal("explicit zero or false was replaced by defaults")
	}
}

func TestNodeValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*NodeConfig)
		want   string
	}{
		{"network mismatch", func(c *NodeConfig) { c.Eth.NetworkId = 100001 }, "conflicts with NetworkID"},
		{"local genesis", func(c *NodeConfig) { c.Eth.Genesis = &core.Genesis{} }, "Eth.Genesis"},
		{"local fork", func(c *NodeConfig) { c.Eth.OverrideOsaka = new(uint64) }, "local fork overrides"},
		{"database engine", func(c *NodeConfig) { c.Node.DBEngine = "unknown" }, "Node.DBEngine"},
		{"state scheme", func(c *NodeConfig) { c.Eth.StateScheme = "unknown" }, "Eth.StateScheme"},
		{"negative peers", func(c *NodeConfig) { c.Node.P2P.MaxPeers = -1 }, "MaxPeers"},
		{"negative cache", func(c *NodeConfig) { c.Eth.DatabaseCache = -1 }, "DatabaseCache"},
		{"http port", func(c *NodeConfig) { c.Node.HTTPPort = 65536 }, "HTTPPort"},
		{"ws port", func(c *NodeConfig) { c.Node.WSPort = -1 }, "WSPort"},
		{"auth port", func(c *NodeConfig) { c.Node.AuthPort = -1 }, "AuthPort"},
		{"listen address", func(c *NodeConfig) { c.Node.P2P.ListenAddr = "invalid" }, "ListenAddr"},
		{"listen port", func(c *NodeConfig) { c.Node.P2P.ListenAddr = ":65536" }, "invalid port"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultNodeConfig()
			test.change(&cfg)
			if err := cfg.Validate(exampleNetwork(t)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
	cfg := DefaultNodeConfig()
	cfg.Node.HTTPPort, cfg.Node.WSPort, cfg.Node.AuthPort = 0, 0, 0
	if err := cfg.Validate(exampleNetwork(t)); err != nil {
		t.Fatalf("port zero must be valid: %v", err)
	}
}
