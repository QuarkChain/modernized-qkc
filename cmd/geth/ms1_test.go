// Copyright 2026 The go-ethereum Authors
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
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func ms1TestNode(t *testing.T, id int) (*eth.Ethereum, *ms1Service, *rpc.Client) {
	t.Helper()
	cfg := gethConfig{Eth: ethconfig.Defaults, Node: defaultNodeConfig()}
	if err := loadConfig(fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", id), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.HTTPHost, cfg.Node.WSHost, cfg.Node.IPCPath = "", "", ""
	cfg.Node.P2P.ListenAddr, cfg.Node.P2P.DiscAddr = "", ""
	cfg.Node.P2P.MaxPeers = 0
	cfg.Node.P2P.NoDial, cfg.Node.P2P.NoDiscovery = true, true
	cfg.Node.P2P.DiscoveryV4, cfg.Node.P2P.DiscoveryV5 = false, false
	cfg.Eth.DatabaseCache, cfg.Eth.TrieCleanCache, cfg.Eth.TrieDirtyCache = 16, 16, 16
	cfg.Eth.SnapshotCache = 0
	stack, err := node.New(&cfg.Node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stack.Close() })
	backend, err := eth.New(stack, &cfg.Eth)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newMS1Service(backend, cfg.Node.CoinbaseAddress)
	if err != nil {
		t.Fatal(err)
	}
	stack.RegisterAPIs([]rpc.API{{Namespace: "ms1", Service: &ms1API{service}}})
	stack.RegisterLifecycle(service)
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	client := stack.Attach()
	t.Cleanup(client.Close)
	return backend, service, client
}

func TestMS1Mining(t *testing.T) {
	data, err := os.ReadFile("../../qkc/config/devnet/root.toml")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Hash              common.Hash
		Header            *qkctypes.RootBlockHeader
		ShardBlockHeaders qkctypes.ShardBlockHeaders
		TrackingData      []byte
	}
	if err := tomlSettings.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root := qkctypes.NewRootBlock(fixture.Header, fixture.ShardBlockHeaders, fixture.TrackingData)
	if root.Hash() != fixture.Hash {
		t.Fatal("fixture hash mismatch")
	}
	encoded, err := root.MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range []int{100001, 100002} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			mining, service, miningRPC := ms1TestNode(t, id)
			engine, ok := mining.Engine().(*posw.PoSWSimulate)
			if !ok {
				t.Fatalf("unexpected engine %T", mining.Engine())
			}
			// Stopping the borrowed wrapper must leave geth's resources usable.
			if err := service.backend.Stop(); err != nil {
				t.Fatal(err)
			}
			if mining.BlockChain().CurrentBlock().Number.Sign() != 0 || service.backend.RootTip() != (common.Hash{}) {
				t.Fatal("mining started before root initialization")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// A root with a different local G0 must fail without initializing the shard.
			wrongHeaders := append(qkctypes.ShardBlockHeaders(nil), root.ShardBlockHeaders()...)
			wrongHeaders[index] = types.CopyHeader(wrongHeaders[index])
			wrongHeaders[index].Root[0] ^= 1
			wrongRoot := qkctypes.NewRootBlock(root.Header(), wrongHeaders, nil)
			wrongData, _ := wrongRoot.MarshalSSZ()
			var hash common.Hash
			if err := miningRPC.CallContext(ctx, &hash, "ms1_initRoot", hexutil.Bytes(wrongData)); err == nil {
				t.Fatal("accepted a mismatched shard genesis")
			}
			if service.backend.RootTip() != (common.Hash{}) {
				t.Fatal("failed initialization changed the root tip")
			}
			heads := make(chan core.ChainHeadEvent, 16)
			sub := mining.BlockChain().SubscribeChainHeadEvent(heads)
			defer sub.Unsubscribe()
			if err := miningRPC.CallContext(ctx, &hash, "ms1_initRoot", hexutil.Bytes(encoded)); err != nil {
				t.Fatal(err)
			}
			if hash != root.Hash() {
				t.Fatal("initialized another root")
			}
			for mining.BlockChain().CurrentBlock().Number.Uint64() < 3 {
				select {
				case <-heads:
				case <-ctx.Done():
					t.Fatal("simulated mining did not produce three blocks:", ctx.Err())
				}
			}
			// Retrying initialization on a running shard must not rewind its head.
			if err := miningRPC.CallContext(ctx, &hash, "ms1_initRoot", hexutil.Bytes(encoded)); err != nil {
				t.Fatal(err)
			}
			if mining.BlockChain().CurrentBlock().Number.Uint64() < 3 {
				t.Fatal("repeated initialization rewound the head")
			}
			if err := miningRPC.CallContext(ctx, &hash, "ms1_initRoot", hexutil.Bytes(wrongData)); err == nil {
				t.Fatal("replaced an initialized root")
			}
			if err := service.Stop(); err != nil {
				t.Fatal(err)
			}
			for number := uint64(1); number <= 3; number++ {
				block := mining.BlockChain().GetBlockByNumber(number)
				if block == nil || block.ParentHash() != mining.BlockChain().GetCanonicalHash(number-1) {
					t.Fatalf("block %d does not extend the canonical chain", number)
				}
				if err := engine.VerifyHeader(mining.BlockChain(), block.Header()); err != nil {
					t.Fatalf("header %d: %v", number, err)
				}
				if err := engine.VerifySeal(mining.BlockChain(), block.Header()); err != nil {
					t.Fatalf("seal %d: %v", number, err)
				}
				extra, err := block.Header().QKCExtra()
				if err != nil || extra.PrevRootBlock != root.Hash() || len(block.Transactions()) != 0 {
					t.Fatalf("unexpected simulated block %d", number)
				}
				if committed, err := qkcrawdb.IsShardBlockCommitted(mining.ChainDb(), block.Hash()); err != nil || !committed {
					t.Fatalf("block %d did not complete the backend import path: %v", number, err)
				}
			}
			// Invalid headers still fail through the same Shard backend/importer.
			head := mining.BlockChain().CurrentBlock()
			candidate, _, err := service.backend.CreateBlockToMine(ctx, head.Coinbase)
			if err != nil {
				t.Fatal(err)
			}
			invalid := candidate.Header()
			invalid.Time = head.Time
			if err := service.backend.InsertMinedBlock(candidate.WithSeal(invalid)); err == nil {
				t.Fatal("accepted a non-increasing timestamp")
			}
			if mining.BlockChain().CurrentBlock().Hash() != head.Hash() {
				t.Fatal("invalid block changed the canonical head")
			}
		})
	}
}
