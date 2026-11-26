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

package master

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/root"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestAPIBackendQueries covers canonical selectors, side-branch hash lookups,
// missing data, unsupported heights and cancellation without requiring readiness.
func TestAPIBackendQueries(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, _ := newTestConfig(t, new(testShard))
	genesis := seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	api := b.APIBackend
	for _, number := range []rpc.BlockNumber{rpc.LatestBlockNumber, rpc.EarliestBlockNumber, 0} {
		header, err := api.HeaderByNumber(t.Context(), number)
		if err != nil || header == nil || header.Hash() != genesis.Hash() {
			t.Fatalf("header %s: %v, %v", number, header, err)
		}
		block, err := api.BlockByNumber(t.Context(), number)
		if err != nil || block == nil || block.Hash() != genesis.Hash() {
			t.Fatalf("block %s: %v, %v", number, block, err)
		}
	}
	for _, number := range []rpc.BlockNumber{
		rpc.PendingBlockNumber, rpc.SafeBlockNumber, rpc.FinalizedBlockNumber, -6, math.MaxUint32 + 1,
	} {
		if _, err := api.HeaderByNumber(t.Context(), number); err == nil {
			t.Fatalf("unsupported header selector accepted: %s", number)
		}
		if _, err := api.BlockByNumber(t.Context(), number); err == nil {
			t.Fatalf("unsupported block selector accepted: %s", number)
		}
	}
	if header, err := api.HeaderByNumber(t.Context(), 1); header != nil || err != nil {
		t.Fatalf("missing header: %v, %v", header, err)
	}
	if block, err := api.BlockByNumber(t.Context(), math.MaxUint32); block != nil || err != nil {
		t.Fatalf("missing block at maximum height: %v, %v", block, err)
	}
	// Store a noncanonical block to distinguish hash lookup from height lookup.
	header := genesis.Header()
	header.Extra = []byte{1}
	side := qkctypes.NewRootBlock(header, nil, nil)
	if err := qkcrawdb.WriteRootBlockWithError(b.db, side); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []common.Hash{genesis.Hash(), side.Hash(), {0xff}} {
		header, headerErr := api.HeaderByHash(t.Context(), hash)
		block, blockErr := api.BlockByHash(t.Context(), hash)
		if headerErr != nil || blockErr != nil {
			t.Fatal(headerErr, blockErr)
		}
		if hash == (common.Hash{0xff}) {
			if header != nil || block != nil {
				t.Fatal("unknown hash returned data")
			}
		} else if header == nil || block == nil || header.Hash() != hash || block.Hash() != hash {
			t.Fatal("hash lookup returned another block")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := api.HeaderByNumber(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query: %v", err)
	}
	if _, err := api.BlockByHash(ctx, genesis.Hash()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hash query: %v", err)
	}
	// Node invokes Backend.Stop only for a started lifecycle.
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	if err := stack.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := api.BlockByNumber(t.Context(), rpc.LatestBlockNumber); !errors.Is(err, context.Canceled) {
		t.Fatalf("query after shutdown: %v", err)
	}
}

// Build valid root fixtures through the existing builder, without adding a
// master mining API or bypassing consensus validation on import.
func nextAPITestBlock(t *testing.T, b *Backend, cfg *Config) *qkctypes.RootBlock {
	t.Helper()
	builder, err := root.NewRootBlockBuilder(cfg.Chain, b.chain, b.engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	block, err := builder.BuildBlock(t.Context(), root.RootBlockArgs{
		Parent: b.chain.RootTip(), ShardHeaders: map[string][]*coretypes.Header{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return block
}

// TestAPIBackendImports checks readiness, consensus error propagation and that
// mined imports advance the root chain.
func TestAPIBackendImports(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, _ := newTestConfig(t, new(testShard))
	genesis := seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	api := b.APIBackend
	if err := api.InsertMinedBlock(genesis); err == nil {
		t.Fatal("mined import accepted before initialization")
	}
	if err := api.SetHead(0); !errors.Is(err, root.ErrNotImplemented) {
		t.Fatalf("SetHead: %v", err)
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	block := nextAPITestBlock(t, b, cfg)
	header := block.Header()
	header.ParentHash = common.Hash{0xff}
	if err := api.InsertMinedBlock(block.WithSeal(header)); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("consensus error was not propagated: %v", err)
	}
	if err := api.InsertMinedBlock(block); err != nil {
		t.Fatal(err)
	}
	got, err := api.BlockByNumber(t.Context(), rpc.LatestBlockNumber)
	if err != nil || got == nil || got.Hash() != block.Hash() {
		t.Fatalf("latest did not advance after import: %v", err)
	}
	if err := stack.Close(); err != nil {
		t.Fatal(err)
	}
	if err := api.InsertMinedBlock(block); !errors.Is(err, context.Canceled) {
		t.Fatalf("import after shutdown: %v", err)
	}
}

// TestMasterRPCQueries checks real node registration, initialization-time reads,
// hex JSON encoding, null results and exclusion of internal mutation methods.
func TestMasterRPCQueries(t *testing.T) {
	stack := newTestNode(t, "pebble")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfg, _ := newTestConfig(t, &testShard{init: func(*qkctypes.RootBlock) error {
		close(entered)
		<-release
		return nil
	}})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	genesis := seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-testContext(t).Done():
		t.Fatal("initialization did not begin")
	}
	if b.Ready() {
		t.Fatal("expected blocked initialization")
	}
	client := stack.Attach()
	defer client.Close()
	for _, query := range []struct {
		method string
		arg    interface{}
		block  bool
	}{
		{"master_getHeaderByNumber", "latest", false},
		{"master_getHeaderByHash", genesis.Hash(), false},
		{"master_getBlockByNumber", "earliest", true},
		{"master_getBlockByHash", genesis.Hash(), true},
	} {
		var got map[string]interface{}
		if err := client.CallContext(testContext(t), &got, query.method, query.arg); err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]interface{}{
			"hash": genesis.Hash().Hex(), "version": "0x0", "number": "0x0",
			"timestamp": "0x1", "difficulty": "0x64", "total_difficulty": "0x64",
			"coinbaseAmount": "0x0", "extraData": "0x", "nonce": "0x0",
			"signature": "0x" + strings.Repeat("00", 65),
		} {
			if got[field] != want {
				t.Fatalf("%s %s: got %v, want %v", query.method, field, got[field], want)
			}
		}
		if query.block {
			headers, ok := got["shardBlockHeaders"].([]interface{})
			if !ok || len(headers) != 1 {
				t.Fatalf("missing shard headers: %v", got["shardBlockHeaders"])
			}
			shard := headers[0].(map[string]interface{})
			if shard["hash"] != genesis.ShardBlockHeaders()[0].Hash().Hex() || shard["number"] != "0x0" {
				t.Fatal("shard header did not use existing JSON encoding")
			}
			if got["trackingData"] != "0x" || got["size"] != hexutil.EncodeUint64(genesis.Size()) {
				t.Fatal("incorrect root body encoding")
			}
		} else if _, ok := got["shardBlockHeaders"]; ok {
			t.Fatal("header query included the block body")
		}
	}
	var result interface{}
	for _, query := range []struct {
		method string
		arg    interface{}
	}{
		{"master_getHeaderByNumber", "0x1"},
		{"master_getBlockByNumber", "0x1"},
		{"master_getHeaderByHash", common.Hash{0xff}},
		{"master_getBlockByHash", common.Hash{0xff}},
	} {
		if err := client.CallContext(testContext(t), &result, query.method, query.arg); err != nil || result != nil {
			t.Fatalf("missing RPC data: %v, %v", result, err)
		}
	}
	for _, selector := range []string{"pending", "safe", "finalized", "0x100000000"} {
		if err := client.CallContext(testContext(t), &result, "master_getBlockByNumber", selector); err == nil {
			t.Fatalf("unsupported selector accepted: %s", selector)
		}
	}
	for _, method := range []string{"master_insertMinedBlock", "master_setHead"} {
		err := client.CallContext(testContext(t), &result, method)
		var rpcErr rpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.ErrorCode() != -32601 {
			t.Fatalf("internal method exposed: %s: %v", method, err)
		}
	}
	once.Do(func() { close(release) })
	waitBackendReady(t, b)
	block := nextAPITestBlock(t, b, cfg)
	if err := b.APIBackend.InsertMinedBlock(block); err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := client.CallContext(testContext(t), &got, "master_getBlockByNumber", "latest"); err != nil {
		t.Fatal(err)
	}
	if got["hash"] != block.Hash().Hex() || got["number"] != "0x1" || len(got["shardBlockHeaders"].([]interface{})) != 0 {
		t.Fatal("latest block or empty shard header list encoded incorrectly")
	}
}
