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

package slave

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

func startSlave(t *testing.T, cfg *config.NodeConfig) (*node.Node, *Slave) {
	t.Helper()
	stack, err := node.New(&cfg.Node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stack.Close() })
	s, err := New(stack, cfg.Eth)
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	return stack, s
}

func TestStandaloneSlave(t *testing.T) {
	for index, scheme := range []string{"hash", "path"} {
		t.Run(scheme, func(t *testing.T) {
			cluster, err := config.LoadCluster("../config/examples/cluster.toml")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &cluster.Slaves[index] // Ethash and simulate respectively.
			if err := cfg.LoadGenesis(); err != nil {
				t.Fatal(err)
			}
			cfg.Node.DataDir, cfg.Node.IPCPath = t.TempDir(), ""
			cfg.Node.HTTPHost, cfg.Node.HTTPPort = "127.0.0.1", 0
			cfg.Node.WSHost, cfg.Node.WSPort = "127.0.0.1", 0
			cfg.Eth.StateScheme = scheme
			cfg.Eth.DatabaseCache, cfg.Eth.DatabaseHandles = 16, 16
			cfg.Eth.TrieCleanCache, cfg.Eth.TrieDirtyCache, cfg.Eth.SnapshotCache = 16, 16, 16
			if scheme == "hash" {
				cfg.Eth.SnapshotCache = 0 // Standalone startup must not require snap sync.
			}
			cfg.Eth.EthDiscoveryURLs = []string{"unused-eth-discovery"}
			cfg.Eth.SnapDiscoveryURLs = []string{"unused-snap-discovery"}
			syncMode := cfg.Eth.SyncMode
			account := common.Address{0x12}
			cfg.Eth.Genesis.Alloc[account] = types.Account{Balance: big.NewInt(12345)}
			if err := cfg.Validate(cluster.NetworkID); err != nil {
				t.Fatal(err)
			}
			stack, s := startSlave(t, cfg)
			if cfg.Eth.SyncMode != syncMode || len(cfg.Eth.EthDiscoveryURLs) != 1 || len(cfg.Eth.SnapDiscoveryURLs) != 1 {
				t.Fatal("startup modified caller's sync or discovery configuration")
			}
			if index == 0 {
				if _, ok := s.Ethereum.Engine().(*posw.PoSW); !ok {
					t.Fatalf("engine %T", s.Ethereum.Engine())
				}
			} else if _, ok := s.Ethereum.Engine().(*posw.PoSWSimulate); !ok {
				t.Fatalf("engine %T", s.Ethereum.Engine())
			}
			if len(s.Ethereum.Protocols()) != 0 {
				t.Fatal("slave advertised Ethereum peer protocols")
			}
			if s.Shard.CurrentBlock() != s.Ethereum.BlockChain().CurrentBlock() {
				t.Fatal("shard did not bind the execution chain")
			}
			if _, err := s.Shard.GetUnconfirmedHeaderList(); !errors.Is(err, shard.ErrRootChainUninitialized) {
				t.Fatalf("unexpected root initialization: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			client, err := ethclient.DialContext(ctx, stack.HTTPEndpoint())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			id, err := client.ChainID(ctx)
			if err != nil || id.Cmp(cfg.ChainID) != 0 {
				t.Fatalf("chain ID: %v, %v", id, err)
			}
			number, err := client.BlockNumber(ctx)
			if err != nil || number != 0 {
				t.Fatalf("block number: %d, %v", number, err)
			}
			balance, err := client.BalanceAt(ctx, account, nil)
			if err != nil || balance.Cmp(big.NewInt(12345)) != 0 {
				t.Fatalf("balance: %v, %v", balance, err)
			}
			var output hexutil.Bytes
			if err := client.Client().CallContext(ctx, &output, "eth_call", map[string]any{"to": account}, "latest"); err != nil {
				t.Fatal(err)
			}
			if logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{}); err != nil || len(logs) != 0 {
				t.Fatalf("logs: %v, %v", logs, err)
			}
			if _, err := client.BlockByNumber(ctx, big.NewInt(int64(rpc.PendingBlockNumber))); err == nil {
				t.Fatal("served an Ethereum pending block for QKC")
			}
			// In-process RPC exposes the same APIs as IPC, including administrative methods.
			local := stack.Attach()
			defer local.Close()
			for method, args := range map[string][]any{"debug_setHead": {"0x0"}, "admin_importChain": {"unused.rlp"}} {
				if err := local.CallContext(ctx, nil, method, args...); err == nil {
					t.Fatalf("%s: %v", method, err)
				}
			}
			if err := s.Ethereum.APIBackend.SetHead(0); err == nil || !strings.Contains(err.Error(), "QKC") {
				t.Fatalf("QKC rewind guard: %v", err)
			}
			ws, err := ethclient.DialContext(ctx, stack.WSEndpoint())
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			sub, err := ws.SubscribeNewHead(ctx, make(chan *types.Header, 1))
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Unsubscribe()
			// The shared native builder can construct QKC blocks after explicit Init.
			root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1)}, qkctypes.ShardBlockHeaders{s.Shard.CurrentBlock()}, nil)
			if err := s.Shard.Init(root); err != nil {
				t.Fatal(err)
			}
			block, _, err := s.Shard.CreateBlockToMine(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			extra, err := block.Header().QKCExtra()
			if err != nil || extra.PrevRootBlock != root.Hash() {
				t.Fatalf("builder root: %v, %v", extra, err)
			}
			if err := s.Shard.AddShardBlock(ctx, block); err == nil {
				t.Fatal("standalone shard accepted a block without cluster delivery")
			}
			if err := stack.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-sub.Err():
			case <-ctx.Done():
				t.Fatal("subscription outlived shutdown")
			}
			if _, err := client.BlockNumber(ctx); err == nil {
				t.Fatal("RPC stayed open after shutdown")
			}
			// Both geth's DB lock and local transaction journal must be released.
			reopened, restored := startSlave(t, cfg)
			if restored.Shard.CurrentBlock().Hash() != s.Shard.CurrentBlock().Hash() {
				t.Fatal("genesis changed on restart")
			}
			restoredClient := ethclient.NewClient(reopened.Attach())
			defer restoredClient.Close()
			balance, err = restoredClient.BalanceAt(ctx, account, nil)
			if err != nil || balance.Cmp(big.NewInt(12345)) != 0 {
				t.Fatalf("persisted balance: %v, %v", balance, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
