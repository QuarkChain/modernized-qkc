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
	"slices"
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
	ClusterClient
	headers map[string][]*types.Header
	err     error
}

func (cluster *builderCluster) GetUnconfirmedHeaderList(context.Context) (map[string][]*types.Header, error) {
	return cluster.headers, cluster.err
}

type builderChain struct {
	poswroot.RootChainReader
	parent    *qkctypes.RootBlockHeader
	roots     map[common.Hash]*qkctypes.RootBlockHeader
	confirmed map[string]common.Hash
	reported  map[common.Hash]*uint256.Int
	reportErr error
	onGetRoot func(common.Hash)
}

func (chain *builderChain) GetRootHeader(hash common.Hash) (*qkctypes.RootBlockHeader, error) {
	if chain.onGetRoot != nil {
		chain.onGetRoot(hash)
	}
	if hash == chain.parent.Hash() {
		return chain.parent, nil
	}
	return chain.roots[hash], nil
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

func newTestBuilder(t *testing.T, cluster ClusterClient) (*RootBlockBuilder, *builderChain) {
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

func (chain *builderChain) shardHeaders(chainID int64, firstHeight int64, firstTime uint64, count int, prevRoots ...common.Hash) []*types.Header {
	id := big.NewInt(chainID)
	parentHash := chain.confirmed[id.String()]
	headers := make([]*types.Header, count)
	for index := range headers {
		prevRoot := chain.parent.Hash()
		if index < len(prevRoots) {
			prevRoot = prevRoots[index]
		}
		header := &types.Header{
			ParentHash: parentHash, Number: big.NewInt(firstHeight + int64(index)), Time: firstTime + uint64(index),
			Difficulty: new(big.Int), Extra: (&types.QKCExtra{ChainID: id, PrevRootBlock: prevRoot}).Encode(),
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

func TestBuildRootBlockStopsAtUnreportedShardHeader(t *testing.T) {
	builder, chain := newTestBuilder(t, nil)
	headers := chain.shardHeaders(2, 10, 100, 4)
	a10, a11, a12, a13 := headers[0], headers[1], headers[2], headers[3]
	chain.confirmed["2"] = a10.Hash()
	delete(chain.reported, a12.Hash())

	x12 := types.CopyHeader(a12)
	x12.Coinbase = common.Address{99}
	a13OnX12 := types.CopyHeader(a13)
	a13OnX12.ParentHash = x12.Hash()
	chain.reported[a13OnX12.Hash()] = chain.reported[a13.Hash()]

	build := func(last *types.Header) common.Hash {
		t.Helper()
		block, err := builder.BuildBlock(t.Context(), RootBlockArgs{
			Parent: chain.parent, Timestamp: 110,
			ShardHeaders: map[string][]*types.Header{"2": {a11, a12, last}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := block.ShardBlockHeaders(); len(got) != 1 || got[0].Hash() != a11.Hash() {
			t.Fatalf("A13 parent %s: got %d shard headers with commitment %s, want only A11", last.ParentHash, len(got), block.ShardHeaderHash())
		}
		return block.Hash()
	}
	control := build(a13)
	if got := build(a13OnX12); got != control {
		t.Fatalf("changing A13's parent changed the root hash from %s to %s", control, got)
	}
}

func TestBuildEmptyRootBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		builder, chain := newTestBuilder(t, nil)
		now := uint64(time.Now().Unix())
		args := RootBlockArgs{Parent: chain.parent, ShardHeaders: map[string][]*types.Header{}}
		for _, parentTime := range []uint64{now - 10, now + 14} {
			chain.parent.Time = parentTime
			block, err := builder.BuildBlock(t.Context(), args)
			if err != nil {
				t.Fatal(err)
			}
			if block.Time() != max(parentTime+1, now) || len(block.ShardBlockHeaders()) != 0 || block.CoinbaseAmount().Uint64() != 11 {
				t.Fatalf("unexpected empty root: time %d, headers %d, reward %v", block.Time(), len(block.ShardBlockHeaders()), block.CoinbaseAmount())
			}
		}
		chain.parent.Time = now + 15
		for _, timestamp := range []uint64{0, now + 60} {
			args.Timestamp = timestamp
			if block, err := builder.BuildBlock(t.Context(), args); block != nil || !errors.Is(err, consensus.ErrFutureBlock) {
				t.Fatalf("timestamp %d: got block %v, error %v; want ErrFutureBlock", timestamp, block, err)
			}
		}
	})
}

func TestBuildRootBlockAlignsShardConfirmations(t *testing.T) {
	builder, chain := newTestBuilder(t, nil)
	headers := chain.shardHeaders(2, 9, 100, 6) // A9 through A14; the parent confirms A10.
	chain.confirmed["2"] = headers[1].Hash()
	other := chain.shardHeaders(3, 21, 100, 1)
	args := RootBlockArgs{Parent: chain.parent, Timestamp: 110, ShardHeaders: map[string][]*types.Header{"3": other}}

	check := func(t *testing.T, candidates, want []*types.Header) {
		t.Helper()
		args.ShardHeaders["2"] = candidates
		block, err := builder.BuildBlock(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		wantHeaders := slices.Concat(want, other)
		if !slices.EqualFunc(block.ShardBlockHeaders(), wantHeaders, func(got, want *types.Header) bool { return got.Hash() == want.Hash() }) {
			t.Fatal("root did not include the expected headers from both shards")
		}
		if err := builder.engine.VerifyBlock(chain, block); err != nil {
			t.Fatalf("constructed root failed consensus: %v", err)
		}
	}
	t.Run("cap counts only new headers", func(t *testing.T) {
		check(t, headers, headers[2:5]) // Include A11, A12, A13; defer A14.
	})
	t.Run("all already confirmed", func(t *testing.T) {
		check(t, headers[:2], nil)
	})
	t.Run("same height on another fork", func(t *testing.T) {
		fork := types.CopyHeader(headers[1])
		fork.Coinbase = common.Address{99}
		child := types.CopyHeader(headers[2])
		child.ParentHash = fork.Hash()
		chain.reported[fork.Hash()], chain.reported[child.Hash()] = uint256.NewInt(1), uint256.NewInt(1)
		check(t, []*types.Header{headers[0], fork, child}, nil)
	})
	t.Run("ignore broken link before confirmation", func(t *testing.T) {
		fork := types.CopyHeader(headers[0])
		fork.Coinbase = common.Address{99}
		check(t, []*types.Header{fork, headers[1], headers[2]}, headers[2:3])
	})
	t.Run("ignore height gap and shard ID before confirmation", func(t *testing.T) {
		otherShardAt8 := chain.shardHeaders(3, 8, 100, 1)[0]
		check(t, []*types.Header{otherShardAt8, headers[1], headers[2]}, headers[2:3])
	})
	t.Run("height gap immediately after confirmation", func(t *testing.T) {
		gap := types.CopyHeader(headers[2])
		gap.Number.Add(gap.Number, common.Big1)
		chain.reported[gap.Hash()] = uint256.NewInt(1)
		args.ShardHeaders["2"] = []*types.Header{headers[0], headers[1], gap}
		if block, err := builder.BuildBlock(t.Context(), args); block != nil || err == nil || !strings.Contains(err.Error(), "nonconsecutive") {
			t.Fatalf("height gap: got block %v, error %v", block, err)
		}
	})
	t.Run("unreported header still truncates selection", func(t *testing.T) {
		delete(chain.reported, headers[3].Hash()) // A12 has not been reported.
		check(t, []*types.Header{headers[0], headers[1], headers[2], headers[3], nil}, headers[2:3])
	})
	t.Run("missing confirmation is an error", func(t *testing.T) {
		delete(chain.confirmed, "2")
		args.ShardHeaders["2"] = headers
		if block, err := builder.BuildBlock(t.Context(), args); block != nil || err == nil {
			t.Fatalf("missing confirmation: got block %v, error %v", block, err)
		}
	})
}

func TestBuildRootBlockRequiresIdleStakingShardConfirmation(t *testing.T) {
	config := params.NewTestQuarkChainConfig(10, 10, 2).QuarkChain
	config.PoSW.StakingEnabled = true
	config.PoSW.WindowSize = 1
	config.PoSW.DiffDivider = 2
	config.PoSW.StakePerBlock = big.NewInt(1)
	engine, err := poswroot.NewSimulate(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	chain := &builderChain{
		parent:    &qkctypes.RootBlockHeader{Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1)},
		confirmed: map[string]common.Hash{"2": {2}},
	}
	builder, err := NewRootBlockBuilder(config, chain, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := RootBlockArgs{Parent: chain.parent, Timestamp: 1, ShardHeaders: map[string][]*types.Header{}}
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || err == nil || !strings.Contains(err.Error(), "shard 10: missing shard confirmation") {
		t.Fatalf("missing staking shard confirmation: got block %v, error %v", block, err)
	}
}

func TestBuildRootBlockFiltersSiblingRoot(t *testing.T) {
	cluster := new(builderCluster)
	builder, chain := newTestBuilder(t, cluster)
	ancestor := &qkctypes.RootBlockHeader{Number: chain.parent.Number - 1, Time: chain.parent.Time - 1}
	chain.parent.ParentHash = ancestor.Hash()
	sibling := qkctypes.CopyRootBlockHeader(chain.parent)
	sibling.Nonce++
	chain.roots = map[common.Hash]*qkctypes.RootBlockHeader{ancestor.Hash(): ancestor, sibling.Hash(): sibling}

	// Both roots confirm the same shard block. The first new header refers to
	// their common ancestor, then the shard follows the sibling root. Its next
	// root is unavailable locally: the builder must stop before looking it up.
	siblingChild := sibling.CreateBlockToAppend(nil, nil, nil, nil, nil)
	headers := chain.shardHeaders(2, 11, 100, 3, ancestor.Hash(), sibling.Hash(), siblingChild.Hash())
	other := chain.shardHeaders(3, 21, 100, 1)
	cluster.headers = map[string][]*types.Header{"2": headers, "3": other}
	args := RootBlockArgs{Parent: chain.parent, Timestamp: 105}
	block, err := builder.BuildBlock(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	want := qkctypes.ShardBlockHeaders{headers[0], other[0]}
	if block.ShardHeaderHash() != qkctypes.CalcShardHeaderHash(want) {
		t.Fatal("expected the prefix before the sibling root and the other shard's header")
	}
	if err := builder.engine.VerifyBlock(chain, block); err != nil {
		t.Fatalf("constructed root failed consensus: %v", err)
	}
	delete(chain.roots, sibling.Hash())
	if block, err := builder.BuildBlock(t.Context(), args); block != nil || !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("missing referenced root: got block %v, error %v", block, err)
	}
}

func TestBuildRootBlockRejectsBrokenLists(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]*types.Header)
		want   string
	}{
		{"height gap", func(headers []*types.Header) { headers[1].Number.Add(headers[1].Number, common.Big1) }, "nonconsecutive"},
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

func TestBuildRootBlockCancelsDuringAncestorWalk(t *testing.T) {
	builder, chain := newTestBuilder(t, nil)
	ancestor := &qkctypes.RootBlockHeader{Number: chain.parent.Number - 2}
	middle := &qkctypes.RootBlockHeader{Number: chain.parent.Number - 1, ParentHash: ancestor.Hash()}
	chain.parent.ParentHash = middle.Hash()
	chain.roots = map[common.Hash]*qkctypes.RootBlockHeader{ancestor.Hash(): ancestor, middle.Hash(): middle}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	chain.onGetRoot = func(hash common.Hash) {
		if ctx.Err() != nil {
			t.Fatalf("read root %s after cancellation", hash)
		}
		if hash == middle.Hash() {
			cancel()
		}
	}
	args := RootBlockArgs{
		Parent: chain.parent, Timestamp: 105,
		ShardHeaders: map[string][]*types.Header{"2": chain.shardHeaders(2, 11, 100, 1, ancestor.Hash())},
	}
	if block, err := builder.BuildBlock(ctx, args); block != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got block %v, error %v; want context.Canceled", block, err)
	}
}
