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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfave/cli/v2"
)

func m1TestConfig(t *testing.T, chainID int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", chainID))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	text := string(data)
	text = strings.Replace(text, "[Eth]", "[Eth]\nDatabaseCache = 16\nTrieCleanCache = 16\nTrieDirtyCache = 16\nSnapshotCache = 0", 1)
	text = strings.Replace(text, fmt.Sprintf("DataDir = %q", fmt.Sprintf("./qkc-data/shard-%d", chainID)), fmt.Sprintf("DataDir = %q", dir), 1)
	text = strings.Replace(text, fmt.Sprintf("IPCPath = %q", fmt.Sprintf("qkc-shard-%d.ipc", chainID)), `IPCPath = ""`, 1)
	text = strings.Replace(text, fmt.Sprintf("HTTPPort = %d", 18545+chainID-100001), "HTTPPort = 0", 1)
	path := filepath.Join(dir, "shard.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestM1ShardRPC(t *testing.T) {
	chainIDs := []int{100001, 100002}
	var stacks []*node.Node
	var backends []*eth.Ethereum
	for _, chainID := range chainIDs {
		stack, backend, err := startM1Shard(m1TestConfig(t, chainID))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stack.Close() })
		stacks = append(stacks, stack)
		backends = append(backends, backend)
	}
	root, err := initM1Shards(backends)
	if err != nil {
		t.Fatal(err)
	}
	for i, chainID := range chainIDs {
		t.Run(fmt.Sprint(chainID), func(t *testing.T) {
			if backends[i].Shard().RootTip() != root.Hash() {
				t.Fatal("shards must share the same genesis root")
			}
			client, err := rpc.DialHTTP(stacks[i].HTTPEndpoint())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var id hexutil.Big
			if err := client.Call(&id, "eth_chainId"); err != nil || id.ToInt().Int64() != int64(chainID) {
				t.Fatalf("chain ID %v: %v", id, err)
			}
			var work shard.MiningWork
			if err := client.Call(&work, "miner_getWork", common.Address{0xaa}); err != nil {
				t.Fatal(err)
			}
			if work.Number != 1 || work.Difficulty.Sign() <= 0 {
				t.Fatalf("unexpected work: %+v", work)
			}
			var accepted bool
			err = client.Call(&accepted, "miner_submitWork", work.SealHash, types.BlockNonce{}, common.Hash{})
			wantHeight := uint64(0)
			if chainID == 100001 {
				if err == nil || !strings.Contains(err.Error(), "invalid Ethash mix hash") || accepted {
					t.Fatalf("Ethash shard accepted an invalid seal: accepted=%t err=%v", accepted, err)
				}
			} else {
				wantHeight = 1
				if err != nil || !accepted {
					t.Fatalf("simulated shard rejected a seal: accepted=%t err=%v", accepted, err)
				}
			}
			var height hexutil.Uint64
			if err := client.Call(&height, "eth_blockNumber"); err != nil || uint64(height) != wantHeight {
				t.Fatalf("head %d, want %d: %v", height, wantHeight, err)
			}
		})
	}
}

func TestM1StartupFailure(t *testing.T) {
	path := m1TestConfig(t, 100001)
	app := &cli.App{Commands: []*cli.Command{m1Command}}
	if err := app.Run([]string{"qkc", "m1", "--config", path, "--config", filepath.Join(t.TempDir(), "missing.toml")}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing shard config: %v", err)
	}
	// A later startup failure must close earlier nodes and release their data directories.
	stack, _, err := startM1Shard(path)
	if err != nil {
		t.Fatal(err)
	}
	stack.Close()
}
