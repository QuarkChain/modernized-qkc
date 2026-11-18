// Copyright 2026 The quarkchain Authors
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

package root

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

type builderCluster struct {
	headers map[string][]*types.Header
	err     error
}

func (cluster *builderCluster) GetUnconfirmedHeaderList(context.Context) (map[string][]*types.Header, error) {
	return cluster.headers, cluster.err
}

type builderChain struct {
	poswroot.RootChainReader
	parent    *qkctypes.RootBlockHeader
	confirmed map[string]common.Hash
	reported  map[common.Hash]*uint256.Int
	reportErr error
}

func (chain *builderChain) GetRootHeader(hash common.Hash) (*qkctypes.RootBlockHeader, error) {
	if hash == chain.parent.Hash() {
		return chain.parent, nil
	}
	return nil, nil
}

func (chain *builderChain) GetLastConfirmedShardBlocks(hash common.Hash) (map[string]common.Hash, error) {
	if hash != chain.parent.Hash() {
		return nil, consensus.ErrUnknownAncestor
	}
	return chain.confirmed, nil
}

func (chain *builderChain) GetShardBlockCoinbaseAmount(hash common.Hash) (*uint256.Int, error) {
	return chain.reported[hash], chain.reportErr
}

func newTestBuilder(t *testing.T, cluster ShardHeaderSource) (*RootBlockBuilder, *builderChain) {
	t.Helper()
	config := params.NewTestQuarkChainConfig(10, 10, 2, 3).QuarkChain
	config.MaxBlocksPerRootBlock = 3
	config.PoSW.CoinbaseAmount = big.NewInt(11)
	engine, err := poswroot.NewSimulate(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	chain := &builderChain{
		parent: &qkctypes.RootBlockHeader{
			Number: 7, Time: 100, Difficulty: *uint256.NewInt(1024), TotalDifficulty: *uint256.NewInt(8192),
			Nonce: 7, MixDigest: common.Hash{8}, Signature: [65]byte{9},
		},
		confirmed: map[string]common.Hash{"2": {2}, "3": {3}, "10": {10}},
		reported:  make(map[common.Hash]*uint256.Int),
	}
	builder, err := NewRootBlockBuilder(config, chain, engine, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if config.ChainIDs[0].Int64() != 10 {
		t.Fatal("builder changed the configured root staking chain")
	}
	return builder, chain
}

func (chain *builderChain) shardHeaders(chainID int64, firstHeight int64, firstTime uint64, count int) []*types.Header {
	id := big.NewInt(chainID)
	parentHash := chain.confirmed[id.String()]
	headers := make([]*types.Header, count)
	for index := range headers {
		header := &types.Header{
			ParentHash: parentHash, Number: big.NewInt(firstHeight + int64(index)), Time: firstTime + uint64(index),
			Difficulty: new(big.Int), Extra: (&types.QKCExtra{ChainID: id, PrevRootBlock: chain.parent.Hash()}).Encode(),
		}
		parentHash = header.Hash()
		chain.reported[parentHash] = uint256.NewInt(1)
		headers[index] = header
	}
	return headers
}

func TestBuildRootBlock(t *testing.T) {
	cluster := new(builderCluster)
	builder, chain := newTestBuilder(t, cluster)
	reportGap := chain.shardHeaders(2, 21, 100, 3)
	chain.reported[reportGap[0].Hash()] = uint256.NewInt(0)
	delete(chain.reported, reportGap[1].Hash())
	future := chain.shardHeaders(3, 9, 105, 2)
	capped := chain.shardHeaders(10, 11, 100, 4)
	cluster.headers = map[string][]*types.Header{"10": capped, "3": future, "2": reportGap}
	args := RootBlockArgs{Parent: chain.parent, Coinbase: common.Address{42}, Timestamp: 105}
	block, err := builder.BuildBlock(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	want := qkctypes.ShardBlockHeaders{reportGap[0], future[0], capped[0], capped[1], capped[2]}
	if block.ShardHeaderHash() != qkctypes.CalcShardHeaderHash(want) {
		t.Fatal("root did not commit to the eligible shard prefixes in numeric chain order")
	}
	if block.Coinbase() != args.Coinbase || block.CoinbaseAmount().Uint64() != 15 {
		t.Fatalf("unexpected coinbase %s or reward %v", block.Coinbase(), block.CoinbaseAmount())
	}
	if block.ParentHash() != chain.parent.Hash() || block.Number() != 8 || block.Time() != args.Timestamp {
		t.Fatal("candidate does not extend the requested parent at the requested time")
	}
	if block.Nonce() != 0 || block.MixDigest() != (common.Hash{}) || block.Signature() != ([65]byte{}) {
		t.Fatal("candidate inherited the parent seal or signature")
	}
	if err := builder.engine.VerifyBlock(chain, block); err != nil {
		t.Fatalf("constructed root failed consensus: %v", err)
	}
	cluster.err = errors.New("unexpected cluster request")
	args.ShardHeaders = cluster.headers
	direct, err := builder.BuildBlock(t.Context(), args)
	if err != nil || direct.Hash() != block.Hash() {
		t.Fatalf("building from the same supplied headers produced a different candidate: %v", err)
	}
}

func TestBuildEmptyRootBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		builder, chain := newTestBuilder(t, nil)
		now := uint64(time.Now().Unix())
		for _, parentTime := range []uint64{now - 10, now + 1} {
			chain.parent.Time = parentTime
			block, err := builder.BuildBlock(t.Context(), RootBlockArgs{Parent: chain.parent, ShardHeaders: map[string][]*types.Header{}})
			if err != nil {
				t.Fatal(err)
			}
			if block.Time() != max(parentTime+1, now) || len(block.ShardBlockHeaders()) != 0 || block.CoinbaseAmount().Uint64() != 11 {
				t.Fatalf("unexpected empty root: time %d, headers %d, reward %v", block.Time(), len(block.ShardBlockHeaders()), block.CoinbaseAmount())
			}
		}
	})
}

func TestBuildRootBlockRejectsBrokenLists(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]*types.Header)
		want   string
	}{
		{"height gap", func(headers []*types.Header) { headers[1].Number.Add(headers[1].Number, common.Big1) }, "nonconsecutive"},
		{"stale checkpoint", func(headers []*types.Header) { headers[0].ParentHash = common.Hash{99} }, "does not extend"},
		{"broken link", func(headers []*types.Header) { headers[1].ParentHash = common.Hash{99} }, "does not extend"},
		{"wrong shard", func(headers []*types.Header) { headers[0].Extra = (&types.QKCExtra{ChainID: big.NewInt(3)}).Encode() }, "belongs to shard"},
		{"nil header", func(headers []*types.Header) { headers[0] = nil }, "index 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder, chain := newTestBuilder(t, nil)
			headers := chain.shardHeaders(2, 11, 100, 2)
			test.mutate(headers)
			for _, header := range headers {
				if header != nil {
					chain.reported[header.Hash()] = uint256.NewInt(1)
				}
			}
			args := RootBlockArgs{Parent: chain.parent, ShardHeaders: map[string][]*types.Header{"2": headers}}
			if block, err := builder.BuildBlock(t.Context(), args); block != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got block %v, error %v; want %q", block, err, test.want)
			}
		})
	}
}

func TestBuildRootBlockErrors(t *testing.T) {
	readErr := errors.New("read failed")
	cluster := &builderCluster{err: readErr}
	builder, chain := newTestBuilder(t, cluster)
	cluster.headers = map[string][]*types.Header{"2": chain.shardHeaders(2, 11, 100, 1)}
	args := RootBlockArgs{Parent: chain.parent, Timestamp: 105}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if block, err := builder.BuildBlock(ctx, args); block != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled build: got %v, %v", block, err)
	}
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || !errors.Is(err, readErr) {
		t.Fatalf("partially failed RPC: got %v, %v", block, err)
	}
	cluster.err = nil
	chain.reportErr = readErr
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || !errors.Is(err, readErr) {
		t.Fatalf("failed shard report lookup: got %v, %v", block, err)
	}
	chain.reportErr = nil
	chain.reported[cluster.headers["2"][0].Hash()] = new(uint256.Int).SetAllOne()
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || err == nil || !strings.Contains(err.Error(), "root coinbase amount exceeds uint256") {
		t.Fatalf("reward overflow in Finalize: got %v, %v", block, err)
	}
	args.Parent = &qkctypes.RootBlockHeader{Time: 1}
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("parent unknown to Prepare: got %v, %v", block, err)
	}
}
