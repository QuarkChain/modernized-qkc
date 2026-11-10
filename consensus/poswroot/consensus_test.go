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

package poswroot

import (
	"bytes"
	"errors"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

type stakeResult struct {
	stakes *uint256.Int
	signer common.Address
}

// The reader has no current-head queries: every result is keyed to a hash.
type testChain struct {
	blocks          map[common.Hash]*qkctypes.RootBlock
	confirmed       map[common.Hash]map[string]*types.Header
	shardAmounts    map[common.Hash]*uint256.Int
	stakes          map[common.Hash]stakeResult
	headerErr       error
	confirmationErr error
	shardAmountErr  error
	stakeErr        error
	headerReads     int
	stakeHash       common.Hash
	stakeChain      *big.Int
	stakeCoinbase   common.Address
}

func (c *testChain) GetRootHeader(hash common.Hash) (*qkctypes.RootBlockHeader, error) {
	c.headerReads++
	if c.headerErr != nil {
		return nil, c.headerErr
	}
	if b := c.blocks[hash]; b != nil {
		return b.Header(), nil
	}
	return nil, nil
}

func (c *testChain) GetLastConfirmedShardBlocks(root common.Hash) (map[string]common.Hash, error) {
	hashes := make(map[string]common.Hash, len(c.confirmed[root]))
	for id, header := range c.confirmed[root] {
		if header != nil {
			hashes[id] = header.Hash()
		}
	}
	return hashes, c.confirmationErr
}

func (c *testChain) GetShardBlockCoinbaseAmount(hash common.Hash) (*uint256.Int, error) {
	return c.shardAmounts[hash], c.shardAmountErr
}

func (c *testChain) GetRootChainStakes(id *big.Int, hash common.Hash, coinbase common.Address) (*uint256.Int, common.Address, error) {
	c.stakeHash, c.stakeChain, c.stakeCoinbase = hash, new(big.Int).Set(id), coinbase
	r := c.stakes[hash]
	return r.stakes, r.signer, c.stakeErr
}

type fixture struct {
	engine *PoSW
	chain  *testChain
	parent *qkctypes.RootBlock
}

// newFixture starts all shards at root genesis. Tests add shard block amounts and confirmation
// records explicitly, without a database, execution state or running cluster.
func newFixture(t *testing.T, configure func(*params.QuarkChainConfig)) *fixture {
	t.Helper()
	config := params.NewTestQuarkChainConfig(10001, 10001, 10002).QuarkChain
	config.RewardTaxRate = big.NewRat(1, 4)
	config.MaxBlocksPerRootBlock = 10
	config.PoSW.MinimumDifficulty = big.NewInt(100)
	config.PoSW.CoinbaseAmount = big.NewInt(101)
	config.PoSW.EpochInterval = 3
	config.PoSW.DecayFactor = big.NewRat(1, 2)
	config.PoSW.WindowSize, config.PoSW.DiffDivider = 4, 10
	config.PoSW.StakePerBlock = big.NewInt(100)
	if configure != nil {
		configure(config)
	}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	chain := &testChain{
		blocks:       make(map[common.Hash]*qkctypes.RootBlock),
		confirmed:    make(map[common.Hash]map[string]*types.Header),
		shardAmounts: make(map[common.Hash]*uint256.Int), stakes: make(map[common.Hash]stakeResult),
	}
	var headers qkctypes.ShardBlockHeaders
	confirmations := make(map[string]*types.Header)
	// Header order follows numeric chain IDs; the config's first ID keeps its
	// separate role as the root staking chain.
	chainIDs := slices.Clone(config.ChainIDs)
	slices.SortFunc(chainIDs, (*big.Int).Cmp)
	for _, id := range chainIDs {
		h := &types.Header{Number: new(big.Int), Difficulty: new(big.Int), Time: 1, Extra: (&types.QKCExtra{ChainID: id}).Encode()}
		headers = append(headers, h)
		confirmations[id.String()] = h
		chain.stakes[h.Hash()] = stakeResult{stakes: new(uint256.Int)}
	}
	parent := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Time: 1, Difficulty: *uint256.NewInt(100), TotalDifficulty: *uint256.NewInt(100),
	}, headers, nil)
	chain.blocks[parent.Hash()], chain.confirmed[parent.Hash()] = parent, confirmations
	t.Cleanup(func() { engine.Close() })
	return &fixture{engine: engine, chain: chain, parent: parent}
}

// verifyUnsealed checks header and body rules without requiring a mined fixture.
// Full VerifyBlock coverage uses fixed Ethash vectors below.
func (f *fixture) verifyUnsealed(block *qkctypes.RootBlock) error {
	header := block.Header()
	parent, err := f.chain.GetRootHeader(header.ParentHash)
	if err != nil {
		return err
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if err := f.engine.verifyHeader(header, parent); err != nil {
		return err
	}
	return f.engine.verifyBody(f.chain, block, parent)
}

func (f *fixture) shard(id int64, amount uint64) *types.Header {
	parent := f.chain.confirmed[f.parent.Hash()][big.NewInt(id).String()]
	return f.extendShard(parent, f.parent.Hash(), amount)
}

// extendShard records a validated shard block's after-tax miner income.
// This alone does not make the header confirmed by a root.
func (f *fixture) extendShard(parent *types.Header, root common.Hash, amount uint64) *types.Header {
	extra, _ := parent.QKCExtra()
	header := &types.Header{
		ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number, common.Big1),
		Difficulty: new(big.Int), Time: f.parent.Time() + 1,
		Extra: (&types.QKCExtra{ChainID: extra.ChainID, PrevRootBlock: root}).Encode(),
	}
	f.chain.shardAmounts[header.Hash()] = uint256.NewInt(amount)
	return header
}

// candidate prepares difficulty and rewards but leaves the block unmined.
func (f *fixture) candidate(t *testing.T, headers ...*types.Header) *qkctypes.RootBlock {
	t.Helper()
	header := &qkctypes.RootBlockHeader{
		Number: f.parent.Number() + 1, ParentHash: f.parent.Hash(), Time: f.parent.Time() + 1,
		Coinbase: common.Address{9},
	}
	if err := f.engine.Prepare(f.chain, header); err != nil {
		t.Fatal(err)
	}
	block, err := f.engine.Finalize(f.chain, qkctypes.NewRootBlock(header, headers, nil))
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func changeHeader(block *qkctypes.RootBlock, edit func(*qkctypes.RootBlockHeader)) *qkctypes.RootBlock {
	header := block.Header()
	edit(header)
	return block.WithSeal(header)
}

// Required configuration must be checked when the engine is created.
func TestNew(t *testing.T) {
	if engine, err := New(nil); err == nil || engine != nil {
		t.Fatal("nil config accepted")
	}
	if engine, err := NewSimulate(nil); err == nil || engine != nil {
		t.Fatal("simulation accepted nil config")
	}
	config := params.NewTestQuarkChainConfig(10001).QuarkChain
	config.RewardTaxRate = nil
	if engine, err := New(config); err == nil || engine != nil {
		t.Fatal("missing tax accepted")
	}
	if engine, err := NewSimulate(config); err == nil || engine != nil {
		t.Fatal("simulation accepted missing tax")
	}
}

// Finalize updates the input block's reward and shard commitment in place while
// preserving its coinbase. Body verification must agree on the resulting reward.
func TestPrepareFinalizeAndVerify(t *testing.T) {
	f := newFixture(t, nil)
	a := f.shard(10001, 1)
	b := f.shard(10002, 2)
	header := &qkctypes.RootBlockHeader{Number: 1, ParentHash: f.parent.Hash(), Time: 2, Coinbase: common.Address{9}}
	if err := f.engine.Prepare(f.chain, header); err != nil {
		t.Fatal(err)
	}
	input := qkctypes.NewRootBlock(header, qkctypes.ShardBlockHeaders{a, b}, []byte("tracking"))
	shardHash := input.ShardHeaderHash()
	before, _ := input.MarshalSSZ()
	oldHash, oldSize := input.Hash(), input.Size()
	block, err := f.engine.Finalize(f.chain, input)
	if err != nil {
		t.Fatal(err)
	}
	// 101 untaxed subsidy + floor((1+2)/3), rather than taxing each shard amount.
	if block.CoinbaseAmount().Uint64() != 102 || block.Coinbase() != header.Coinbase || block.ShardHeaderHash() != shardHash {
		t.Fatal("Finalize set the wrong reward, coinbase or shard commitment")
	}
	if err := f.verifyUnsealed(block); err != nil {
		t.Fatal(err)
	}
	after, _ := input.MarshalSSZ()
	if block != input || bytes.Equal(before, after) || oldHash == input.Hash() || oldSize != input.Size() {
		t.Fatal("Finalize did not update the input block in place or refresh its cached hash")
	}
	if author, err := f.engine.Author(block.Header()); err != nil || author != header.Coinbase {
		t.Fatal("Author did not return coinbase")
	}
	// Validation rejects an incorrect commitment; assembly recomputes it.
	bad := changeHeader(input, func(h *qkctypes.RootBlockHeader) { h.ShardHeaderHash = common.Hash{1} })
	badHash := bad.Hash()
	if err := f.verifyUnsealed(bad); err == nil {
		t.Fatal("invalid commitment accepted")
	}
	finalized, err := f.engine.Finalize(f.chain, bad)
	if err != nil || finalized != bad || finalized.ShardHeaderHash() != shardHash || bad.Hash() == badHash {
		t.Fatal("Finalize did not recompute the commitment and hash in place")
	}
	if err := f.verifyUnsealed(finalized); err != nil {
		t.Fatalf("finalized block rejected: %v", err)
	}
}

// Check root fields before PoW so an invalid seal cannot hide a missed check.
// Verification must also leave rejected blocks unchanged.
func TestVerifyRootFields(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*qkctypes.RootBlockHeader)
	}{
		{"version", func(h *qkctypes.RootBlockHeader) { h.Version = 1 }},
		{"genesis", func(h *qkctypes.RootBlockHeader) { h.Number = 0 }},
		{"height", func(h *qkctypes.RootBlockHeader) { h.Number++ }},
		{"parent", func(h *qkctypes.RootBlockHeader) { h.ParentHash = common.Hash{1} }},
		{"old time", func(h *qkctypes.RootBlockHeader) { h.Time = 1 }},
		{"future", func(h *qkctypes.RootBlockHeader) { h.Time = uint64(time.Now().Unix()) + 60 }},
		{"difficulty", func(h *qkctypes.RootBlockHeader) { h.Difficulty.AddUint64(&h.Difficulty, 1) }},
		{"total difficulty", func(h *qkctypes.RootBlockHeader) { h.TotalDifficulty.AddUint64(&h.TotalDifficulty, 1) }},
		{"amount", func(h *qkctypes.RootBlockHeader) { h.CoinbaseAmount.AddUint64(&h.CoinbaseAmount, 1) }},
		{"commitment", func(h *qkctypes.RootBlockHeader) { h.ShardHeaderHash = common.Hash{1} }},
		{"extra", func(h *qkctypes.RootBlockHeader) { h.Extra = make([]byte, 1025) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, nil)
			block := changeHeader(f.candidate(t), test.edit)
			before, _ := block.MarshalSSZ()
			if err := f.verifyUnsealed(block); err == nil {
				t.Fatal("invalid root accepted")
			}
			after, _ := block.MarshalSSZ()
			if !bytes.Equal(before, after) {
				t.Fatal("header or body verification changed its input")
			}
		})
	}
	t.Run("future boundary", func(t *testing.T) {
		// Freeze time without changing the engine's clock or racing a real second.
		synctest.Test(t, func(t *testing.T) {
			f := newFixture(t, nil)
			boundary := f.candidate(t).Header()
			boundary.Time = uint64(time.Now().Unix()) + 15
			if err := f.engine.verifyHeader(boundary, f.parent.Header()); err != nil {
				t.Fatalf("future time boundary: %v", err)
			}
			boundary.Time++
			if err := f.engine.verifyHeader(boundary, f.parent.Header()); !errors.Is(err, consensus.ErrFutureBlock) {
				t.Fatal("future time limit not enforced")
			}
		})
	})
	f := newFixture(t, nil)
	block := f.candidate(t).WithBody(nil, make([]byte, 1025))
	if err := f.verifyUnsealed(block); err == nil {
		t.Fatal("oversized tracking data accepted")
	}
	block = changeHeader(f.candidate(t), func(h *qkctypes.RootBlockHeader) { h.Extra = make([]byte, 1024) }).WithBody(nil, make([]byte, 1024))
	if err := f.verifyUnsealed(block); err != nil {
		t.Fatalf("size boundary: %v", err)
	}
	if err := f.engine.VerifyBlock(f.chain, nil); err == nil {
		t.Fatal("nil root accepted")
	}
	if err := f.engine.VerifyBlock(nil, f.candidate(t)); err == nil {
		t.Fatal("nil reader accepted")
	}
	missingParent := changeHeader(f.candidate(t), func(h *qkctypes.RootBlockHeader) { h.ParentHash = common.Hash{1} })
	if err := f.engine.VerifyBlock(f.chain, missingParent); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("missing parent error %v, want ErrUnknownAncestor", err)
	}
}

// Shard validation alone is not enough: headers must satisfy ordering, limits,
// parent-root ancestry and continuation from the parent's confirmation.
func TestShardHeaderRules(t *testing.T) {
	for _, name := range []string{"order", "limit", "parent", "time", "unknown chain", "root fork", "unknown block", "missing amount", "missing confirmation"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *params.QuarkChainConfig) { c.MaxBlocksPerRootBlock = 1 })
			a, b := f.shard(10001, 1), f.shard(10002, 2)
			block := f.candidate(t, a, b)
			headers := qkctypes.ShardBlockHeaders{a, b}
			switch name {
			case "order":
				headers = qkctypes.ShardBlockHeaders{b, a}
			case "limit":
				headers = qkctypes.ShardBlockHeaders{a, f.extendShard(a, f.parent.Hash(), 2)}
			case "parent":
				a.ParentHash = common.Hash{1}
			case "time":
				a.Time = block.Time() + 1
			case "unknown chain":
				a.Extra = (&types.QKCExtra{ChainID: big.NewInt(10000), PrevRootBlock: f.parent.Hash()}).Encode()
			case "root fork":
				fork := changeHeader(f.parent, func(h *qkctypes.RootBlockHeader) { h.Extra = []byte("fork") })
				f.chain.blocks[fork.Hash()] = fork
				a.Extra = (&types.QKCExtra{ChainID: big.NewInt(10001), PrevRootBlock: fork.Hash()}).Encode()
			case "unknown block":
				delete(f.chain.shardAmounts, a.Hash())
			case "missing amount":
				f.chain.shardAmounts[a.Hash()] = nil
			case "missing confirmation":
				delete(f.chain.confirmed[f.parent.Hash()], "10001")
			}
			if name != "unknown block" && name != "missing amount" {
				f.chain.shardAmounts[a.Hash()] = uint256.NewInt(1)
			}
			want := map[string]string{
				"order": "grouped", "limit": "too many", "parent": "continue",
				"time": "timestamp", "unknown chain": "unknown shard", "root fork": "ancestor",
				"unknown block": "unknown shard block", "missing amount": "unknown shard block", "missing confirmation": "missing parent",
			}[name]
			if err := f.verifyUnsealed(qkctypes.NewRootBlock(block.Header(), headers, nil)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v, want %q", err, want)
			}
		})
	}
}

// After the first header joins the parent's confirmation, every later header
// in the same group must continue both the height and parent hash.
func TestShardHeadersWithinGroup(t *testing.T) {
	f := newFixture(t, nil)
	// Start from a non-genesis confirmation to check the parent-root boundary
	// as well as links between headers inside the candidate.
	confirmed := f.shard(10001, 1)
	parent := f.candidate(t, confirmed)
	f.chain.blocks[parent.Hash()] = parent
	f.chain.confirmed[parent.Hash()] = maps.Clone(f.chain.confirmed[f.parent.Hash()])
	f.chain.confirmed[parent.Hash()]["10001"] = confirmed
	f.parent = parent
	first := f.shard(10001, 1)
	next := f.extendShard(first, f.parent.Hash(), 1)
	block := f.candidate(t, first, next)
	if err := f.verifyUnsealed(block); err != nil {
		t.Fatalf("valid shard sequence: %v", err)
	}
	for _, name := range []string{"parent", "height"} {
		t.Run(name, func(t *testing.T) {
			header := types.CopyHeader(next)
			if name == "parent" {
				header.ParentHash = common.Hash{1}
			} else {
				header.Number.Add(header.Number, common.Big1)
			}
			f.chain.shardAmounts[header.Hash()] = uint256.NewInt(1)
			candidate := qkctypes.NewRootBlock(block.Header(), qkctypes.ShardBlockHeaders{first, header}, nil)
			if err := f.verifyUnsealed(candidate); err == nil || !strings.Contains(err.Error(), "continue") {
				t.Fatalf("error %v, want invalid within-group link", err)
			}
		})
	}
}

// Unknown headers must fail the shard block lookup before their fields are used.
// Nil and zero Difficulty have the same RLP hash and resolve to the same amount.
// A non-nil zero amount must still identify a known block.
func TestShardBlockCoinbaseAmountLookup(t *testing.T) {
	f := newFixture(t, nil)
	block := f.candidate(t, f.shard(10001, 0))
	for _, test := range []struct {
		name string
		edit func(*types.Header) *types.Header
	}{
		{"nil header", func(h *types.Header) *types.Header { return nil }},
		{"nil number", func(h *types.Header) *types.Header { h.Number = nil; return h }},
		{"nil difficulty", func(h *types.Header) *types.Header { h.Difficulty = nil; return h }},
		{"negative number", func(h *types.Header) *types.Header { h.Number = big.NewInt(-1); return h }},
		{"negative difficulty", func(h *types.Header) *types.Header { h.Difficulty = big.NewInt(-1); return h }},
		{"negative base fee", func(h *types.Header) *types.Header { h.BaseFee = big.NewInt(-1); return h }},
		{"large number", func(h *types.Header) *types.Header { h.Number = new(big.Int).Lsh(common.Big1, 64); return h }},
		{"invalid extra", func(h *types.Header) *types.Header { h.Extra = nil; return h }},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Mutate the owned body so CopyHeader does not normalize nil fields.
			candidate := block.WithSeal(block.Header())
			headers := candidate.ShardBlockHeaders()
			headers[0] = test.edit(headers[0])
			if headers[0] != nil {
				// Keep the commitment valid so verification reaches the shard lookup.
				coinbase := candidate.Coinbase()
				candidate.Finalize(candidate.CoinbaseAmount(), &coinbase)
			}
			verifyErr := f.verifyUnsealed(candidate)
			finalized, finalizeErr := f.engine.Finalize(f.chain, candidate)
			if test.name == "nil difficulty" {
				if verifyErr != nil || finalizeErr != nil {
					t.Fatalf("equivalent shard header rejected: verify %v, finalize %v", verifyErr, finalizeErr)
				}
				if headers[0].Difficulty != nil || finalized.CoinbaseAmount().Cmp(block.CoinbaseAmount()) != 0 {
					t.Fatal("shard coinbase amount or candidate input changed")
				}
				return
			}
			want := "unknown shard block"
			if test.name == "nil header" {
				want = "missing shard header"
			}
			if verifyErr == nil || !strings.Contains(verifyErr.Error(), want) || finalizeErr == nil || !strings.Contains(finalizeErr.Error(), want) {
				t.Fatalf("verify %v, finalize %v; want %q", verifyErr, finalizeErr, want)
			}
		})
	}
}

// A nil Number can hash like a known shard genesis header. Body checks must
// reject its invalid confirmation continuation without changing the input.
func TestKnownShardNilNumberCannotContinueConfirmation(t *testing.T) {
	f := newFixture(t, nil)
	genesis := f.chain.confirmed[f.parent.Hash()]["10001"]
	f.chain.shardAmounts[genesis.Hash()] = new(uint256.Int)
	first := f.shard(10001, 0)
	block := qkctypes.NewRootBlock(f.candidate(t, first).Header(), qkctypes.ShardBlockHeaders{genesis, first}, nil)
	block.ShardBlockHeaders()[0].Number = nil
	if err := f.verifyUnsealed(block); err == nil || !strings.Contains(err.Error(), "continue") {
		t.Fatalf("error %v, want invalid confirmation continuation", err)
	}
	if block.ShardBlockHeaders()[0].Number != nil {
		t.Fatal("verification changed the candidate header")
	}
}

// Failed reads must reach the caller instead of being treated as absent data.
func TestReaderFailures(t *testing.T) {
	failure := errors.New("read failed")
	for _, name := range []string{"header", "confirmation", "shard amount"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			block := f.candidate(t, f.shard(10001, 1))
			switch name {
			case "header":
				f.chain.headerErr = failure
			case "confirmation":
				f.chain.confirmationErr = failure
			case "shard amount":
				f.chain.shardAmountErr = failure
			}
			if err := f.verifyUnsealed(block); !errors.Is(err, failure) {
				t.Fatalf("read error lost: %v", err)
			}
		})
	}
}

// Reject values that cannot fit in stored fields, but allow reward calculations
// whose intermediate sum exceeds uint256 when the final amount still fits.
func TestDifficultyAndRewardOverflow(t *testing.T) {
	f := newFixture(t, nil)
	rewardBlock := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{Number: 3}, qkctypes.ShardBlockHeaders{f.shard(10001, 3)}, nil)
	if amount, err := f.engine.coinbaseAmount(f.chain, rewardBlock); err != nil || amount.Uint64() != 51 {
		t.Fatal("wrong decayed root subsidy or tax rounding")
	}
	for _, field := range []string{"difficulty", "total"} {
		t.Run(field, func(t *testing.T) {
			parent := changeHeader(f.parent, func(h *qkctypes.RootBlockHeader) {
				switch field {
				case "difficulty":
					h.Difficulty.SetAllOne()
				case "total":
					h.TotalDifficulty.SetAllOne()
				}
			})
			f.chain.blocks[parent.Hash()] = parent
			header := &qkctypes.RootBlockHeader{Number: parent.Number() + 1, ParentHash: parent.Hash(), Time: parent.Time() + 1}
			if err := f.engine.Prepare(f.chain, header); err == nil {
				t.Fatal("overflow accepted")
			}
		})
	}
	f = newFixture(t, func(c *params.QuarkChainConfig) {
		c.RewardTaxRate = big.NewRat(1, 2)
		c.PoSW.CoinbaseAmount = new(uint256.Int).SetAllOne().ToBig()
	})
	shard := f.shard(10001, 1)
	header := &qkctypes.RootBlockHeader{Number: 1, ParentHash: f.parent.Hash(), Time: 2}
	if err := f.engine.Prepare(f.chain, header); err != nil {
		t.Fatal(err)
	}
	block := qkctypes.NewRootBlock(header, qkctypes.ShardBlockHeaders{shard}, nil)
	before, _ := block.MarshalSSZ()
	oldHash := block.Hash()
	if result, err := f.engine.Finalize(f.chain, block); err == nil || result != nil {
		t.Fatal("Finalize accepted amount overflow")
	}
	after, _ := block.MarshalSSZ()
	if !bytes.Equal(before, after) || block.Hash() != oldHash {
		t.Fatal("failed Finalize changed its input block")
	}
	if err := f.verifyUnsealed(block); err == nil {
		t.Fatal("body verification accepted amount overflow")
	}
	f = newFixture(t, func(c *params.QuarkChainConfig) { c.PoSW.CoinbaseAmount = new(big.Int) })
	headers := qkctypes.ShardBlockHeaders{f.shard(10001, 0), f.shard(10002, 0)}
	for _, header := range headers {
		f.chain.shardAmounts[header.Hash()] = new(uint256.Int).SetAllOne()
	}
	large := new(big.Int).Mul(new(uint256.Int).SetAllOne().ToBig(), big.NewInt(2))
	rewardBlock = qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{Number: 1}, headers, nil)
	amount, err := f.engine.coinbaseAmount(f.chain, rewardBlock)
	if err != nil || amount.ToBig().Cmp(new(big.Int).Div(large, big.NewInt(3))) != 0 {
		t.Fatal("intermediate uint256 overflow changed the tax calculation")
	}
	rewardBlock = qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{Number: 3}, nil, nil)
	if amount, err := f.engine.coinbaseAmount(f.chain, rewardBlock); err != nil || !amount.IsZero() {
		t.Fatal("zero subsidy changed")
	}
}

// An invalid root proof must not trigger body ancestry walks, even when known
// shard blocks reference an old root on the candidate's parent branch.
func TestInvalidSealSkipsBodyHistory(t *testing.T) {
	f := newFixture(t, nil)
	genesis := f.parent
	for range 6 {
		f.appendRoot(t, common.Address{})
	}
	header := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10001"], genesis.Hash(), 1)
	block := f.candidate(t, header)
	f.chain.headerReads = 0
	if err := f.engine.VerifyBlock(f.chain, block); err == nil || !strings.Contains(err.Error(), "mix hash") {
		t.Fatalf("error %v, want invalid Ethash mix hash", err)
	}
	if f.chain.headerReads != 1 {
		t.Fatalf("invalid seal caused %d root header reads, want only the parent read", f.chain.headerReads)
	}
}

// Body checks must reject an unknown shard block before its old root reference
// can trigger a walk of the parent branch.
func TestUnknownShardBlockSkipsAncestorWalk(t *testing.T) {
	f := newFixture(t, nil)
	genesis := f.parent
	for range 6 {
		f.appendRoot(t, common.Address{})
	}
	header := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10001"], genesis.Hash(), 1)
	block := f.candidate(t, header)
	delete(f.chain.shardAmounts, header.Hash())
	f.chain.headerReads = 0
	if err := f.verifyUnsealed(block); err == nil || !strings.Contains(err.Error(), "unknown shard block") {
		t.Fatalf("error %v, want unknown shard block", err)
	}
	// Only the candidate's parent may be read before the shard block amount is checked.
	if f.chain.headerReads != 1 {
		t.Fatalf("unknown shard block caused %d root header reads, want 1", f.chain.headerReads)
	}
}

// Several shards may reference the same old root; verify that reference only once.
func TestShardHeadersCanReferenceSameAncestor(t *testing.T) {
	f := newFixture(t, nil)
	genesis := f.parent
	for range 6 {
		f.appendRoot(t, common.Address{})
	}
	a := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10001"], genesis.Hash(), 1)
	b := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10002"], genesis.Hash(), 2)
	block := f.candidate(t, a, b)
	f.chain.headerReads = 0
	if err := f.verifyUnsealed(block); err != nil {
		t.Fatal(err)
	}
	// One read gets the parent, one gets genesis, and six walk the branch.
	// The second shard reuses the verified root hash.
	if f.chain.headerReads != 8 {
		t.Fatalf("root header reads %d, want 8", f.chain.headerReads)
	}
}

// Each shard's root reference must be on the parent branch, whether the older
// or newer reference appears first in the shard list.
func TestRootReferencesAtDifferentHeights(t *testing.T) {
	for _, heights := range [][2]int{{2, 5}, {5, 2}} {
		f := newFixture(t, nil)
		roots := []*qkctypes.RootBlock{f.parent}
		for range 6 {
			roots = append(roots, f.appendRoot(t, common.Address{}))
		}
		a := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10001"], roots[heights[0]].Hash(), 1)
		b := f.extendShard(f.chain.confirmed[f.parent.Hash()]["10002"], roots[heights[1]].Hash(), 2)
		block := f.candidate(t, a, b)
		if err := f.verifyUnsealed(block); err != nil {
			t.Fatal(err)
		}
		// The cache is local to one verification; missing history on a later
		// call must still return an error.
		delete(f.chain.blocks, roots[4].Hash())
		if err := f.verifyUnsealed(block); !errors.Is(err, consensus.ErrUnknownAncestor) {
			t.Fatalf("missing root history error %v, want ErrUnknownAncestor", err)
		}
	}
}

// Simulation accepts unmined roots while keeping header, body, reward and staking
// data checks. The normal constructor must still require a valid Ethash proof.
func TestSimulateVerifyBlock(t *testing.T) {
	for _, name := range []string{"ordinary PoW", "staking"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *params.QuarkChainConfig) { c.PoSW.StakingEnabled = name == "staking" })
			engine, err := NewSimulate(f.engine.config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { engine.Close() })
			block := f.candidate(t, f.shard(10001, 1))
			if name == "staking" {
				block = authorize(t, f, block, 100)
			}
			if err := engine.VerifyBlock(f.chain, block); err != nil {
				t.Fatalf("simulated root rejected: %v", err)
			}
			if name == "staking" && f.chain.stakeHash != f.chain.confirmed[f.parent.Hash()]["10001"].Hash() {
				t.Fatal("simulation did not read parent-confirmed staking state")
			}
			if err := f.engine.VerifyBlock(f.chain, block); err == nil || !strings.Contains(err.Error(), "mix hash") {
				t.Fatalf("normal PoSW accepted an unmined root: %v", err)
			}
			for _, test := range []struct {
				block *qkctypes.RootBlock
				want  string
			}{
				{changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Time = f.parent.Time() }), "timestamp"},
				{block.WithBody(nil, nil), "shard header hash"},
				{changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.CoinbaseAmount.AddUint64(&h.CoinbaseAmount, 1) }), "coinbase amount"},
			} {
				if err := engine.VerifyBlock(f.chain, test.block); err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error %v, want %q", err, test.want)
				}
			}
			if name == "staking" {
				// Simulation must not hide missing state or confirmation records.
				failure := errors.New("staking read failed")
				f.chain.stakeErr = failure
				if err := engine.VerifyBlock(f.chain, block); !errors.Is(err, failure) {
					t.Fatalf("staking error lost: %v", err)
				}
				delete(f.chain.confirmed[f.parent.Hash()], "10001")
				if err := engine.VerifyBlock(f.chain, block); err == nil || !strings.Contains(err.Error(), "missing parent root staking shard confirmation") {
					t.Fatalf("missing confirmation error lost: %v", err)
				}
			}
		})
	}
}

// Exercise the full VerifyBlock path with a real Ethash proof, then change each
// seal input to confirm that verification uses the candidate header's values.
func TestRealEthashIsChecked(t *testing.T) {
	f := newFixture(t, nil)
	block := f.candidate(t)
	if err := f.engine.VerifyBlock(f.chain, block); err == nil || !strings.Contains(err.Error(), "mix hash") {
		t.Fatalf("invalid Ethash seal accepted: %v", err)
	}
	// Fixed epoch-0 Ethash vector for this root fixture, with base difficulty 100.
	if block.Header().SealHash() != common.HexToHash("0x3635a46064e1ea9cf4ed89c276f2b7cbd8e231f50975c973918f10d153688440") {
		t.Fatal("root seal vector fixture changed")
	}
	sealed := block.WithMiningResult(14, common.HexToHash("0xb578552c5fffb785f605bc931ae720068dd2d77e3c31a02894dd47288ead6789"), nil)
	if err := f.engine.VerifyBlock(f.chain, sealed); err != nil {
		t.Fatalf("valid root Ethash seal rejected: %v", err)
	}
	// Tracking data is outside SealHash; valid PoW must still reach body checks.
	if err := f.engine.VerifyBlock(f.chain, sealed.WithBody(nil, make([]byte, 1025))); err == nil || !strings.Contains(err.Error(), "tracking data") {
		t.Fatalf("valid seal bypassed body validation: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*qkctypes.RootBlockHeader)
	}{
		{"nonce", func(h *qkctypes.RootBlockHeader) { h.Nonce++ }},
		{"mix digest", func(h *qkctypes.RootBlockHeader) { h.MixDigest[0] ^= 1 }},
		{"seal hash", func(h *qkctypes.RootBlockHeader) { h.Extra = []byte("changed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := f.engine.VerifyBlock(f.chain, changeHeader(sealed, test.edit)); err == nil {
				t.Fatal("changed Ethash input accepted")
			}
		})
	}
}

var _ consensus.RootChainReader = (*testChain)(nil)
