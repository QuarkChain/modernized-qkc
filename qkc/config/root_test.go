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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

func writeRootDeployment(t *testing.T, filename string, edit func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"root.toml", "shard-100001.toml", "shard-100002.toml"} {
		data, err := os.ReadFile(filepath.Join("devnet", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == filename {
			changed := edit(string(data))
			if changed == string(data) {
				t.Fatalf("fixture edit did not change %s", name)
			}
			data = []byte(changed)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "root.toml")
}

func TestLoadRoot(t *testing.T) {
	path := writeRootDeployment(t, "root.toml", func(s string) string {
		return strings.Replace(s, `"shard-100001.toml", "shard-100002.toml"`, `"shard-100002.toml", "shard-100001.toml"`, 1)
	})
	cfg, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkID != 2 || cfg.Node.CoinbaseAddress == (common.Address{}) || cfg.Genesis.Coinbase != (common.Address{}) {
		t.Fatal("incorrect network or genesis/runtime coinbase")
	}
	if cfg.Genesis.Timestamp != 0 || cfg.Genesis.Difficulty.Cmp(big.NewInt(1024)) != 0 ||
		cfg.Genesis.Config.ConsensusType != params.ConsensusSimulate || cfg.Genesis.Config.PoSW == nil {
		t.Fatalf("unexpected root genesis: %+v", cfg.Genesis)
	}
	if len(cfg.Shards) != 2 {
		t.Fatalf("got %d shards", len(cfg.Shards))
	}
	for i, id := range []int64{100001, 100002} {
		g, slave := cfg.Shards[i].Genesis, cfg.Shards[i].Slave
		if g.Config.ChainID.Cmp(big.NewInt(id)) != 0 || slave.ChainID.Cmp(big.NewInt(id)) != 0 ||
			slave.Host != "127.0.0.1" || int(slave.Port) != 38000+i || !cfg.Genesis.Config.EqualNetworkRules(g.Config.QuarkChain) {
			t.Fatalf("shard %d: got genesis for %v, endpoint %+v", id, g.Config.ChainID, slave)
		}
	}
}

func TestLoadRootWithoutGenesis(t *testing.T) {
	path := writeRootDeployment(t, "root.toml", func(s string) string {
		return strings.Split(s, "[Root.Genesis]")[0]
	})
	cfg, err := LoadRoot(path)
	if err != nil || cfg.Genesis != nil || len(cfg.Shards) != 2 {
		t.Fatalf("got %v, %v", cfg, err)
	}
}

func TestInvalidRootDeployment(t *testing.T) {
	const shard = "shard-100002.toml"
	const root = "root.toml"
	for _, tc := range []struct {
		name, file, old, new, want string
	}{
		{"zero root network", root, "NetworkID = 2", "NetworkID = 0", "NetworkID must be positive"},
		{"invalid root listener", root, `ListenAddr = ":30303"`, `ListenAddr = ":70000"`, "invalid port"},
		{"shard-only root setting", root, "[Node]", "[Node]\nPrivatePort = 38000", "only shards"},
		{"missing shard file", root, shard, "missing.toml", "missing.toml"},
		{"missing member", root, `, "shard-100002.toml"`, "", "missing shard config for chain ID 100002"},
		{"duplicate member", root, shard, "shard-100001.toml", "duplicate shard chain ID"},
		{"fork override", shard, "[Eth]", "[Eth]\nOverrideOsaka = 0", "local fork overrides"},
		{"network", shard, "NetworkId = 2", "NetworkId = 1", "differs from root"},
		{"private host", shard, `PrivateHost = "127.0.0.1"`, `PrivateHost = "0.0.0.0"`, "PrivateHost"},
		{"private port", shard, "PrivatePort = 38001", "PrivatePort = 65536", "PrivatePort"},
		{"duplicate endpoint", shard, "PrivatePort = 38001", "PrivatePort = 38000", "duplicate shard private endpoint"},
		{"chain IDs", shard, `"100001", "100002"`, `"100001", "100002", "100003"`, "must agree across shards"},
		{"tax", shard, `RewardTaxRate = "0.5"`, `RewardTaxRate = "0.4"`, "must agree across shards"},
		{"block limit", shard, "MaxBlocksPerRootBlock = 18", "MaxBlocksPerRootBlock = 19", "must agree across shards"},
		{"invalid shard genesis", shard, "GasLimit = 12000000", "GasLimit = 10000", "invalid gas limit"},
		{"missing timestamp", root, "Timestamp = 0", "", "Timestamp is required"},
		{"difficulty overflow", root, `Difficulty = "1024"`, `Difficulty = "` + new(big.Int).Lsh(big.NewInt(1), 256).String() + `"`, "positive uint256"},
		{"difficulty below minimum", root, `Difficulty = "1024"`, `Difficulty = "1023"`, "below MinimumDifficulty"},
		{"invalid root consensus", root, `"POW_SIMULATE"`, `"NONE"`, "ConsensusType"},
		{"invalid root PoSW", root, "AdjustmentCutoff = 7", "AdjustmentCutoff = 0", "posw"},
		{"duplicate network rule", root, "[Root.Genesis.Config]", "[Root.Genesis.Config]\nMaxBlocksPerRootBlock = 18", "MaxBlocksPerRootBlock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRootDeployment(t, tc.file, func(s string) string {
				return strings.Replace(s, tc.old, tc.new, 1)
			})
			if _, err := LoadRoot(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
