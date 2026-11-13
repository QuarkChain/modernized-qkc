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

func qkcTestNode(t *testing.T, id int) (*eth.Ethereum, *node.Node) {
	t.Helper()
	cfg := gethConfig{Eth: ethconfig.Defaults, Node: defaultNodeConfig()}
	if err := loadConfig(fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", id), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.HTTPHost, cfg.Node.WSHost, cfg.Node.IPCPath = "", "", ""
	cfg.Node.PrivatePort = 0 // Let the OS choose an isolated cluster port.
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
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	return backend, stack
}

type qkcHeaderHandler func(*big.Int, *types.Header, *uint256.Int) error

func (f qkcHeaderHandler) AddShardBlockHeader(id *big.Int, header *types.Header, amount *uint256.Int) error {
	return f(id, header, amount)
}

func qkcTestConnection(t *testing.T, backend *eth.Ethereum, handler qkcHeaderHandler) *masterconn.SlaveConn {
	t.Helper()
	socket, err := net.DialTimeout("tcp", backend.SlaveService().Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := masterconn.NewSlaveConn(masterconn.SlaveConnConfig{
		Conn: socket, RemoteChainID: backend.BlockChain().Config().ChainID, Handler: handler, ShutdownMaster: func() {},
	})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	peer.Start()
	return peer
}

func TestQKCMining(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type report struct {
		header *types.Header
		amount *uint256.Int
	}
	var (
		backends      [2]*eth.Ethereum
		stacks        [2]*node.Node
		peers         [2]*masterconn.SlaveConn
		reports       [2]chan report
		received      [2]map[uint64]report
		rejectReports [2]atomic.Bool
		slaves        []comm.SlaveInfo
	)
	for index, id := range []int{100001, 100002} {
		backend, stack := qkcTestNode(t, id)
		backends[index], stacks[index] = backend, stack
		if backend.BlockChain().CurrentBlock().Number.Sign() != 0 || backend.Shard().RootTip() != (common.Hash{}) {
			t.Fatal("mining started before root initialization")
		}
		wrongHeaders := append(qkctypes.ShardBlockHeaders(nil), root.ShardBlockHeaders()...)
		wrongHeaders[index] = types.CopyHeader(wrongHeaders[index])
		wrongHeaders[index].Root[0] ^= 1
		wrongRoot := qkctypes.NewRootBlock(root.Header(), wrongHeaders, nil)
		if err := backend.Shard().Init(wrongRoot); err == nil {
			t.Fatal("accepted a mismatched shard genesis")
		}
		if backend.Shard().RootTip() != (common.Hash{}) {
			t.Fatal("failed initialization changed the root tip")
		}
		reports[index] = make(chan report, 32)
		peers[index] = qkcTestConnection(t, backend, func(source *big.Int, header *types.Header, amount *uint256.Int) error {
			if source.Int64() != int64(id) {
				return fmt.Errorf("unexpected source chain %s", source)
			}
			if rejectReports[index].Load() {
				return &comm.RemoteError{Code: 7}
			}
			select {
			case reports[index] <- report{header, amount}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err := peers[index].SendPing(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if backend.Shard().RootTip() != (common.Hash{}) || backend.SlaveService().ConnectionsReady() {
			t.Fatal("identity PING initialized the shard or bypassed interconnection")
		}
		addr := backend.SlaveService().Addr().(*net.TCPAddr)
		slaves = append(slaves, comm.SlaveInfo{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: big.NewInt(int64(id))})
	}
	// Every master connection exists before sequential slave interconnection.
	for _, peer := range peers {
		if err := peer.SendConnectToSlaves(ctx, slaves); err != nil {
			t.Fatal(err)
		}
	}
	for index, backend := range backends {
		if !backend.SlaveService().ConnectionsReady() || backend.Shard().RootTip() != (common.Hash{}) {
			t.Fatal("interconnection must finish without initializing the shard")
		}
		if err := peers[index].SendPing(ctx, root); err != nil {
			t.Fatal(err)
		}
		if backend.Shard().RootTip() != root.Hash() {
			t.Fatal("initialized another root")
		}
	}
	for index, backend := range backends {
		received[index] = make(map[uint64]report)
		for len(received[index]) < 3 {
			select {
			case reported := <-reports[index]:
				received[index][reported.header.Number.Uint64()] = reported
			case <-ctx.Done():
				t.Fatal("simulated mining did not report three blocks:", ctx.Err())
			}
		}
		backend.PoWMiner().StopLocalMining()
	}
	for index, backend := range backends {
		engine, ok := backend.Engine().(*posw.PoSWSimulate)
		if !ok {
			t.Fatalf("unexpected engine %T", backend.Engine())
		}
		for number := uint64(1); number <= 3; number++ {
			block := backend.BlockChain().GetBlockByNumber(number)
			if block == nil || block.ParentHash() != backend.BlockChain().GetCanonicalHash(number-1) {
				t.Fatalf("block %d does not extend the canonical chain", number)
			}
			if err := engine.VerifyHeader(backend.BlockChain(), block.Header()); err != nil {
				t.Fatalf("header %d: %v", number, err)
			}
			if err := engine.VerifySeal(backend.BlockChain(), block.Header()); err != nil {
				t.Fatalf("seal %d: %v", number, err)
			}
			extra, err := block.Header().QKCExtra()
			if err != nil || extra.PrevRootBlock != root.Hash() || len(block.Transactions()) != 0 {
				t.Fatalf("unexpected simulated block %d", number)
			}
			if committed, err := qkcrawdb.IsShardBlockCommitted(backend.ChainDb(), block.Hash()); err != nil || !committed {
				t.Fatalf("block %d did not complete delivery: %v", number, err)
			}
			if deposits := qkcrawdb.ReadXShardDeposits(backends[1-index].ChainDb(), block.Hash()); deposits == nil || len(deposits) != 0 {
				t.Fatalf("block %d empty deposit list was not delivered to the other shard", number)
			}
			reported, ok := received[index][number]
			if !ok || reported.header.Hash() != block.Hash() {
				t.Fatalf("reported header %d differs from the canonical block", number)
			}
			amount, err := backend.BlockChain().GetCoinbaseAmount(block.Hash())
			if err != nil || !reported.amount.Eq(amount) {
				t.Fatalf("reported coinbase amount %d differs: %v", number, err)
			}
		}
		// Production initialization rejects repeated checkpoints without rewinding.
		head := backend.BlockChain().CurrentBlock()
		if err := backend.Shard().Init(root); err == nil || backend.BlockChain().CurrentBlock().Hash() != head.Hash() {
			t.Fatal("repeated initialization was accepted or rewound the head")
		}
		candidate, _, err := backend.Shard().CreateBlockToMine(ctx, head.Coinbase)
		if err != nil {
			t.Fatal(err)
		}
		invalid := candidate.Header()
		invalid.Time = head.Time
		if err := backend.Shard().InsertMinedBlock(candidate.WithSeal(invalid)); err == nil {
			t.Fatal("accepted a non-increasing timestamp")
		}
		if backend.BlockChain().CurrentBlock().Hash() != head.Hash() {
			t.Fatal("invalid block changed the canonical head")
		}
		rejectReports[index].Store(true)
		err = backend.Shard().InsertMinedBlock(candidate)
		var remoteErr *comm.RemoteError
		if !errors.As(err, &remoteErr) || remoteErr.Code != 7 {
			t.Fatalf("master rejection was not propagated: %v", err)
		}
		if committed, err := qkcrawdb.IsShardBlockCommitted(backend.ChainDb(), candidate.Hash()); err != nil || committed {
			t.Fatalf("rejected header report was marked committed: %v", err)
		}
		rejectReports[index].Store(false)
		if err := backend.PoWMiner().StartLocalMining(); err != nil {
			t.Fatal(err)
		}
	}
	// One master disconnect also stops the other shard through their real link.
	peers[0].Close()
	for _, stack := range stacks {
		closed := make(chan struct{})
		go func() { stack.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-ctx.Done():
			t.Fatal("cluster disconnect did not shut down both shards")
		}
	}
}
