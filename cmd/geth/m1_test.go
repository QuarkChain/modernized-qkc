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
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/qkc/masterconn"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

func m1TestNode(t *testing.T, id int) (*eth.Ethereum, *m1Service, *node.Node) {
	t.Helper()
	cfg := gethConfig{Eth: ethconfig.Defaults, Node: defaultNodeConfig()}
	if err := loadConfig(fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", id), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.HTTPHost, cfg.Node.WSHost, cfg.Node.IPCPath = "", "", ""
	cfg.Node.PrivatePort = 0 // Let the OS choose an isolated cluster port.
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
	service := newM1Service(stack, &cfg.Eth)
	backend, err := eth.New(stack, &cfg.Eth)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.register(backend); err != nil {
		t.Fatal(err)
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	return backend, service, stack
}

type m1HeaderHandler func(*big.Int, *types.Header, *uint256.Int) error

func (f m1HeaderHandler) AddShardBlockHeader(id *big.Int, header *types.Header, amount *uint256.Int) error {
	return f(id, header, amount)
}

func m1TestConnection(t *testing.T, service *m1Service, handler m1HeaderHandler) *masterconn.SlaveConn {
	t.Helper()
	socket, err := net.DialTimeout("tcp", service.listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := masterconn.NewSlaveConn(masterconn.SlaveConnConfig{
		Conn: socket, RemoteChainID: service.chainID, Handler: handler, ShutdownMaster: func() {},
	})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	peer.Start()
	return peer
}

func TestM1Mining(t *testing.T) {
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
	for index, id := range []int{100001, 100002} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			mining, service, stack := m1TestNode(t, id)
			engine, ok := mining.Engine().(*posw.PoSWSimulate)
			if !ok {
				t.Fatalf("unexpected engine %T", mining.Engine())
			}
			if service.backend != mining.Shard() || service.miner != mining.PoWMiner() {
				t.Fatal("M1 must reuse Ethereum's shard and miner")
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
			if err := service.CreateShards(wrongRoot); err == nil {
				t.Fatal("accepted a mismatched shard genesis")
			}
			if service.backend.RootTip() != (common.Hash{}) {
				t.Fatal("failed initialization changed the root tip")
			}
			type report struct {
				header *types.Header
				amount *uint256.Int
			}
			reports := make(chan report, 16)
			var rejectReports atomic.Bool
			peer := m1TestConnection(t, service, func(source *big.Int, header *types.Header, amount *uint256.Int) error {
				if source.Int64() != int64(id) {
					return fmt.Errorf("unexpected source chain %s", source)
				}
				if rejectReports.Load() {
					return &comm.RemoteError{Code: 7}
				}
				select {
				case reports <- report{header, amount}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err := peer.SendPing(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if service.backend.RootTip() != (common.Hash{}) {
				t.Fatal("identity-only PING initialized the shard")
			}
			if err := peer.SendPing(ctx, root); err != nil {
				t.Fatal(err)
			}
			if service.backend.RootTip() != root.Hash() {
				t.Fatal("initialized another root")
			}
			received := make(map[uint64]report)
			for len(received) < 3 {
				select {
				case reported := <-reports:
					received[reported.header.Number.Uint64()] = reported
				case <-ctx.Done():
					t.Fatal("simulated mining did not report three blocks:", ctx.Err())
				}
			}
			// Retrying initialization on a running shard must not rewind its head.
			if err := peer.SendPing(ctx, root); err != nil {
				t.Fatal(err)
			}
			if mining.BlockChain().CurrentBlock().Number.Uint64() < 3 {
				t.Fatal("repeated initialization rewound the head")
			}
			if err := service.CreateShards(wrongRoot); err == nil {
				t.Fatal("replaced an initialized root")
			}
			service.miner.StopLocalMining()
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
				reported, ok := received[number]
				if !ok || reported.header.Hash() != block.Hash() {
					t.Fatalf("reported header %d differs from the canonical block", number)
				}
				amount, err := mining.BlockChain().GetCoinbaseAmount(block.Hash())
				if err != nil || !reported.amount.Eq(amount) {
					t.Fatalf("reported coinbase amount %d differs: %v", number, err)
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
			// A rejected report must reach the importer and leave delivery uncommitted.
			rejectReports.Store(true)
			err = service.backend.InsertMinedBlock(candidate)
			var remoteErr *comm.RemoteError
			if !errors.As(err, &remoteErr) || remoteErr.Code != 7 {
				t.Fatalf("master rejection was not propagated: %v", err)
			}
			if committed, err := qkcrawdb.IsShardBlockCommitted(mining.ChainDb(), candidate.Hash()); err != nil || committed {
				t.Fatalf("rejected header report was marked committed: %v", err)
			}
			rejectReports.Store(false)
			// Closing qkc's side must shut down geth, including its local miner.
			if err := service.miner.StartLocalMining(); err != nil {
				t.Fatal(err)
			}
			peer.Close()
			closed := make(chan struct{})
			go func() { stack.Wait(); close(closed) }()
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("master disconnect did not shut down the shard")
			}
		})
	}
}
