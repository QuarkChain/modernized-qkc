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
	"maps"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	"github.com/ethereum/go-ethereum/qkc/master"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

type testClient struct {
	headers map[string][]*types.Header
	err     error
	calls   int
}

var _ master.ClusterClient = (*testClient)(nil)

func (c *testClient) BroadcastRootBlock(context.Context, *qkctypes.RootBlock) error {
	return errors.New("unexpected broadcast")
}

func (c *testClient) GetUnconfirmedHeaderList(ctx context.Context) (map[string][]*types.Header, error) {
	c.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.headers, c.err
}

func (c *testClient) GetRootChainStakes(context.Context, *big.Int, common.Address, common.Hash) (*clusternet.RootChainStakesResult, error) {
	return nil, errors.New("unexpected stakes query")
}

// testChain keeps roots, cumulative confirmations and recorded shard income by hash.
type testChain struct {
	roots     map[common.Hash]*qkctypes.RootBlockHeader
	confirmed map[common.Hash]map[string]common.Hash
	amounts   map[common.Hash]*uint256.Int
	amountErr error
}

func (c *testChain) GetRootHeader(hash common.Hash) (*qkctypes.RootBlockHeader, error) {
	if header := c.roots[hash]; header != nil {
		return qkctypes.CopyRootBlockHeader(header), nil
	}
	return nil, nil
}

func (c *testChain) GetLastConfirmedShardBlocks(hash common.Hash) (map[string]common.Hash, error) {
	return c.confirmed[hash], nil
}

func (c *testChain) GetShardBlockCoinbaseAmount(hash common.Hash) (*uint256.Int, error) {
	return c.amounts[hash], c.amountErr
}

func (c *testChain) GetRootChainStakes(*big.Int, common.Hash, common.Address) (*uint256.Int, common.Address, error) {
	return nil, common.Address{}, errors.New("unexpected stakes read")
}

type fixture struct {
	t       *testing.T
	engine  *poswroot.PoSW
	chain   *testChain
	client  *testClient
	builder *Builder
	genesis *qkctypes.RootBlockHeader
	shards  map[int64]*types.Header // Shard genesis headers confirmed by root genesis.
}

// newFixture lists chain IDs out of order to check that root bodies use
// ascending chain IDs. The simulated engine verifies unsealed candidates.
func newFixture(t *testing.T, configure func(*params.QuarkChainConfig)) *fixture {
	t.Helper()
	config := params.NewTestQuarkChainConfig(10002, 10002, 10001).QuarkChain
	config.RewardTaxRate = big.NewRat(1, 4)
	config.PoSW.CoinbaseAmount = big.NewInt(1000)
	if configure != nil {
		configure(config)
	}
	engine, err := poswroot.NewSimulate(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f := &fixture{
		t: t, engine: engine, client: new(testClient), shards: make(map[int64]*types.Header),
		chain: &testChain{
			roots:     make(map[common.Hash]*qkctypes.RootBlockHeader),
			confirmed: make(map[common.Hash]map[string]common.Hash),
			amounts:   make(map[common.Hash]*uint256.Int),
		},
	}
	if f.builder, err = NewBuilder(config, engine, f.client); err != nil {
		t.Fatal(err)
	}
	f.genesis = &qkctypes.RootBlockHeader{Time: 1, Difficulty: *uint256.NewInt(1000), TotalDifficulty: *uint256.NewInt(1000)}
	confirmed := make(map[string]common.Hash)
	for _, id := range config.ChainIDs {
		header := &types.Header{Number: new(big.Int), Difficulty: new(big.Int), Time: 1, Extra: (&types.QKCExtra{ChainID: id}).Encode()}
		f.shards[id.Int64()] = header
		confirmed[id.String()] = header.Hash()
	}
	f.chain.roots[f.genesis.Hash()], f.chain.confirmed[f.genesis.Hash()] = f.genesis, confirmed
	return f
}

// addRoot stores a root on parent that also confirms the given shard headers.
func (f *fixture) addRoot(parent *qkctypes.RootBlockHeader, time uint64, confirms ...*types.Header) *qkctypes.RootBlockHeader {
	f.t.Helper()
	header := &qkctypes.RootBlockHeader{Number: parent.Number + 1, ParentHash: parent.Hash(), Time: time}
	if err := f.engine.Prepare(f.chain, header); err != nil {
		f.t.Fatal(err)
	}
	confirmed := maps.Clone(f.chain.confirmed[parent.Hash()])
	for _, h := range confirms {
		extra, _ := h.QKCExtra()
		confirmed[extra.ChainID.String()] = h.Hash()
	}
	f.chain.roots[header.Hash()], f.chain.confirmed[header.Hash()] = header, confirmed
	return header
}

// next creates a shard block on parent at time that references root. Master
// records the block's number as its income.
func (f *fixture) next(parent *types.Header, root common.Hash, time uint64) *types.Header {
	extra, _ := parent.QKCExtra()
	header := &types.Header{
		ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number, common.Big1),
		Difficulty: new(big.Int), Time: time,
		Extra: (&types.QKCExtra{ChainID: extra.ChainID, PrevRootBlock: root}).Encode(),
	}
	f.chain.amounts[header.Hash()] = uint256.NewInt(header.Number.Uint64())
	return header
}

// extend creates count shard blocks on parent that reference root, each one
// second after its parent.
func (f *fixture) extend(parent *types.Header, root common.Hash, count int) []*types.Header {
	headers := make([]*types.Header, count)
	for i := range headers {
		parent = f.next(parent, root, parent.Time+1)
		headers[i] = parent
	}
	return headers
}

// build creates a candidate on parent at time 100 paying address 9.
func (f *fixture) build(parent *qkctypes.RootBlockHeader) (*qkctypes.RootBlock, error) {
	return f.builder.BuildBlock(context.Background(), f.chain, BlockArgs{Parent: parent.Hash(), Timestamp: 100, Coinbase: common.Address{9}})
}

func (f *fixture) mustBuild(parent *qkctypes.RootBlockHeader, want ...*types.Header) *qkctypes.RootBlock {
	f.t.Helper()
	block, err := f.build(parent)
	if err != nil {
		f.t.Fatal(err)
	}
	got := block.ShardBlockHeaders()
	if len(got) != len(want) {
		f.t.Fatalf("included %d shard headers, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Hash() != want[i].Hash() {
			f.t.Fatalf("shard header %d is #%d of %x, want #%d of %x", i, got[i].Number, got[i].Extra[4:36], want[i].Number, want[i].Extra[4:36])
		}
	}
	if err := f.engine.VerifyBlock(f.chain, block); err != nil {
		f.t.Fatalf("candidate rejected: %v", err)
	}
	return block
}

func TestNewBuilder(t *testing.T) {
	f := newFixture(t, nil)
	config := params.NewTestQuarkChainConfig(10001).QuarkChain
	if b, err := NewBuilder(nil, f.engine, f.client); err == nil || b != nil {
		t.Fatal("missing config accepted")
	}
	if b, err := NewBuilder(config, nil, f.client); err == nil || b != nil {
		t.Fatal("missing engine accepted")
	}
	if b, err := NewBuilder(config, f.engine, nil); err == nil || b != nil {
		t.Fatal("missing client accepted")
	}
}

// The candidate fills header fields through the engine and groups the collected
// headers by ascending chain ID, ignoring unconfigured chains.
func TestBuildBlock(t *testing.T) {
	f := newFixture(t, nil)
	parent := f.addRoot(f.genesis, 10)
	a := f.extend(f.shards[10001], parent.Hash(), 2)
	b := f.extend(f.shards[10002], f.genesis.Hash(), 1)
	f.client.headers = map[string][]*types.Header{"10002": b, "10001": a, "99999": {nil}}
	block := f.mustBuild(parent, a[0], a[1], b[0])

	header := block.Header()
	difficulty := f.engine.CalcDifficulty(100, parent)
	total := new(big.Int).Add(parent.TotalDifficulty.ToBig(), difficulty)
	if header.Number != 2 || header.ParentHash != parent.Hash() || header.Time != 100 || header.Coinbase != (common.Address{9}) {
		t.Fatalf("wrong height, parent, time or coinbase: %+v", header)
	}
	if header.Difficulty.ToBig().Cmp(difficulty) != 0 || header.TotalDifficulty.ToBig().Cmp(total) != 0 {
		t.Fatal("wrong difficulty or total difficulty")
	}
	// Subsidy 1000 plus floor((1+2+1) * 1/3) restored from after-tax shard income.
	if header.CoinbaseAmount.Uint64() != 1001 {
		t.Fatalf("coinbase amount %d, want 1001", header.CoinbaseAmount.Uint64())
	}
	if header.Nonce != 0 || header.MixDigest != (common.Hash{}) || header.Signature != ([65]byte{}) || len(block.TrackingData()) != 0 {
		t.Fatal("candidate is sealed or has tracking data")
	}
	if f.client.calls != 1 {
		t.Fatalf("collected headers %d times", f.client.calls)
	}
}

func TestBuildBlockDefaultTimestamp(t *testing.T) {
	f := newFixture(t, nil)
	before := uint64(time.Now().Unix())
	block, err := f.builder.BuildBlock(context.Background(), f.chain, BlockArgs{Parent: f.genesis.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	if block.Time() < before || block.Time() > uint64(time.Now().Unix()) {
		t.Fatalf("time %d is not the current time", block.Time())
	}
	// A parent ahead of the clock moves the default one second past it.
	future := f.addRoot(f.genesis, uint64(time.Now().Unix())+5)
	block, err = f.builder.BuildBlock(context.Background(), f.chain, BlockArgs{Parent: future.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	if block.Time() != future.Time+1 {
		t.Fatalf("time %d, want %d", block.Time(), future.Time+1)
	}
}

// Headers the parent already confirms are skipped. A shard answering from another
// root tip, whose list does not continue the parent's confirmation, is left out.
func TestBuildBlockFollowsParentConfirmation(t *testing.T) {
	f := newFixture(t, nil)
	a := f.extend(f.shards[10001], f.genesis.Hash(), 3)
	b := f.extend(f.shards[10002], f.genesis.Hash(), 1)
	fork := f.next(f.shards[10002], f.genesis.Hash(), 5)
	parent := f.addRoot(f.genesis, 10, a[0], b[0])

	f.client.headers = map[string][]*types.Header{"10001": a, "10002": {fork, f.next(fork, f.genesis.Hash(), 6)}}
	f.mustBuild(parent, a[1], a[2])
	// Nothing is left when the parent confirms the whole list.
	f.client.headers = map[string][]*types.Header{"10001": a[:1], "10002": b}
	f.mustBuild(parent)
}

// Selection stops before the first header that root validation would reject or
// that master has not recorded; later headers are left for the next root.
func TestBuildBlockStopsAtUnusableHeader(t *testing.T) {
	t.Run("limit", func(t *testing.T) {
		f := newFixture(t, func(c *params.QuarkChainConfig) { c.MaxBlocksPerRootBlock = 2 })
		a := f.extend(f.shards[10001], f.genesis.Hash(), 3)
		f.client.headers = map[string][]*types.Header{"10001": a}
		f.mustBuild(f.genesis, a[0], a[1])
	})
	t.Run("time", func(t *testing.T) {
		f := newFixture(t, nil)
		first := f.next(f.shards[10001], f.genesis.Hash(), 100)
		late := f.next(first, f.genesis.Hash(), 101)
		f.client.headers = map[string][]*types.Header{"10001": {first, late, f.next(late, f.genesis.Hash(), 102)}}
		f.mustBuild(f.genesis, first)
	})
	t.Run("unrecorded", func(t *testing.T) {
		f := newFixture(t, nil)
		a := f.extend(f.shards[10001], f.genesis.Hash(), 3)
		b := f.extend(f.shards[10002], f.genesis.Hash(), 1)
		delete(f.chain.amounts, a[1].Hash())
		f.client.headers = map[string][]*types.Header{"10001": a, "10002": b}
		f.mustBuild(f.genesis, a[0], b[0])
	})
	t.Run("root outside parent branch", func(t *testing.T) {
		f := newFixture(t, nil)
		parent := f.addRoot(f.genesis, 10)
		side := f.addRoot(f.genesis, 11)
		a := f.extend(f.shards[10001], parent.Hash(), 1)
		a = append(a, f.extend(a[0], side.Hash(), 1)...)
		b := f.extend(f.shards[10002], common.Hash{1}, 1) // Unknown root.
		f.client.headers = map[string][]*types.Header{"10001": a, "10002": b}
		f.mustBuild(parent, a[0])
	})
}

func TestBuildBlockErrors(t *testing.T) {
	f := newFixture(t, nil)
	a := f.extend(f.shards[10001], f.genesis.Hash(), 3)
	b := f.extend(f.shards[10002], f.genesis.Hash(), 1)
	expectInconsistent := func(name string, headers map[string][]*types.Header) {
		t.Helper()
		f.client.headers = headers
		if _, err := f.build(f.genesis); !errors.Is(err, errInconsistentHeaders) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	expectInconsistent("height gap", map[string][]*types.Header{"10001": {a[0], a[2]}})
	expectInconsistent("hash link", map[string][]*types.Header{"10001": {a[0], {
		ParentHash: common.Hash{1}, Number: big.NewInt(2), Difficulty: new(big.Int), Time: 3, Extra: a[0].Extra,
	}}})
	expectInconsistent("other chain", map[string][]*types.Header{"10001": b})
	expectInconsistent("nil header", map[string][]*types.Header{"10001": {a[0], nil}})
	expectInconsistent("bad extra", map[string][]*types.Header{"10001": {{Number: big.NewInt(1)}}})

	// Header and parent errors come from the engine before the cluster is queried.
	f.client.calls = 0
	if _, err := f.builder.BuildBlock(context.Background(), f.chain, BlockArgs{Parent: common.Hash{1}, Timestamp: 100}); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("unknown parent: got %v", err)
	}
	if _, err := f.builder.BuildBlock(context.Background(), f.chain, BlockArgs{Parent: f.genesis.Hash(), Timestamp: 1}); err == nil {
		t.Fatal("timestamp not after parent accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.builder.BuildBlock(ctx, f.chain, BlockArgs{Parent: f.genesis.Hash(), Timestamp: 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: got %v", err)
	}
	if _, err := f.builder.BuildBlock(context.Background(), nil, BlockArgs{Parent: f.genesis.Hash(), Timestamp: 100}); err == nil {
		t.Fatal("missing reader accepted")
	}
	if f.client.calls != 0 {
		t.Fatal("cluster queried for an invalid header")
	}

	f.client.headers, f.client.err = nil, errors.New("rpc failed")
	if _, err := f.build(f.genesis); !errors.Is(err, f.client.err) {
		t.Fatalf("client error: got %v", err)
	}
	f.client.headers, f.client.err = map[string][]*types.Header{"10001": a}, nil
	f.chain.amountErr = errors.New("read failed")
	if _, err := f.build(f.genesis); !errors.Is(err, f.chain.amountErr) {
		t.Fatalf("income read error: got %v", err)
	}
	f.chain.amountErr = nil
	delete(f.chain.confirmed[f.genesis.Hash()], "10001")
	if _, err := f.build(f.genesis); err == nil {
		t.Fatal("missing parent confirmation accepted")
	}
}
