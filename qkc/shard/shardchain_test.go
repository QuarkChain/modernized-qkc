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

package shard

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// noVerifyEngine is the beacon engine without header verification, as the
// beacon engine rejects the QuarkChain extra data.
type noVerifyEngine struct {
	consensus.Engine
}

func (noVerifyEngine) VerifyHeader(consensus.ChainHeaderReader, *types.Header) error {
	return nil
}

func (noVerifyEngine) VerifyHeaders(_ consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	results := make(chan error, len(headers))
	for range headers {
		results <- nil
	}
	return make(chan struct{}), results
}

// testEnv is a shard chain on top of a root genesis block, which is the root
// tip until a test sets another one.
type testEnv struct {
	t      *testing.T
	engine consensus.Engine
	config *params.ChainConfig
	genDb  ethdb.Database // Database to generate the blocks with
	db     ethdb.Database // Database of the chain and the root blocks
	chain  *core.BlockChain
	sc     *ShardChain

	rootGenesis *qkctypes.RootBlock
	genesis     *types.Block
}

func newTestEnv(t *testing.T) *testEnv {
	config := *params.MergedTestChainConfig
	config.QuarkChain = &params.QuarkChainConfig{ShardID: 0, ShardIDs: []uint32{0, 1}}
	env := &testEnv{
		t:      t,
		engine: noVerifyEngine{beacon.New(ethash.NewFaker())},
		config: &config,
		db:     rawdb.NewMemoryDatabase(),
		rootGenesis: qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{
			Difficulty:      *uint256.NewInt(1),
			TotalDifficulty: *uint256.NewInt(1),
		}),
	}
	env.addRoot(env.rootGenesis, nil)
	env.setRootTip(env.rootGenesis)

	gspec := &core.Genesis{
		Config:     env.config,
		Difficulty: common.Big0,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		ExtraData:  qkcExtra(env.rootGenesis),
		Alloc:      core.SystemContractAllocs(),
	}
	env.genDb, _, _ = core.GenerateChainWithGenesis(gspec, env.engine, 0, nil)

	chain, err := core.NewBlockChain(env.db, gspec, env.engine, nil)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	t.Cleanup(chain.Stop)
	env.chain = chain
	env.genesis = chain.Genesis()
	env.sc = NewShardChain(env.db, chain)
	return env
}

// qkcExtra returns the extra data of a shard block referencing the given root
// block.
func qkcExtra(prevRoot *qkctypes.RootBlock) []byte {
	return (&types.QKCExtra{PrevRootBlock: prevRoot.Hash()}).Encode()
}

// newRoot creates a root block on top of parent, carrying the given shard block
// headers. Blocks with different seeds are different.
func newRoot(parent *qkctypes.RootBlock, seed uint64, headers ...*types.Header) *qkctypes.RootBlock {
	block := parent.Header().CreateBlockToAppend(nil, nil, nil, &seed, nil)
	return qkctypes.NewRootBlock(block.Header(), headers, nil)
}

// remoteHeader returns the header of a block of another shard referencing
// prevRoot. Headers with different numbers are different.
func remoteHeader(shard uint32, number int64, prevRoot *qkctypes.RootBlock) *types.Header {
	return &types.Header{
		Number:     big.NewInt(number),
		Difficulty: common.Big0,
		Extra:      (&types.QKCExtra{ShardID: shard, PrevRootBlock: prevRoot.Hash()}).Encode(),
	}
}

// addRoot stores a root block, along with the latest shard block it confirms
// if any.
func (env *testEnv) addRoot(root *qkctypes.RootBlock, confirmed *types.Block) {
	qkcrawdb.WriteRootBlock(env.db, root)
	if confirmed != nil {
		qkcrawdb.WriteLastConfirmedBlockHash(env.db, root.Hash(), confirmed.Hash())
	}
}

func (env *testEnv) setRootTip(root *qkctypes.RootBlock) {
	qkcrawdb.WriteRootHeadHash(env.db, root.Hash())
}

// storeXShardDeposits stores the cross-shard deposits sent to the shard by a
// block of another shard, as an empty list.
func (env *testEnv) storeXShardDeposits(header *types.Header) {
	key := append([]byte("qkc_xShard"), header.Hash().Bytes()...)
	if err := env.db.Put(key, []byte{0xc0}); err != nil { // RLP encoding of an empty list
		env.t.Fatalf("failed to store cross-shard deposits: %v", err)
	}
}

// insertRoot inserts a root block, and checks whether it became the root tip.
func (env *testEnv) insertRoot(root *qkctypes.RootBlock, wantSwitched bool) {
	env.t.Helper()
	switched, err := env.sc.InsertRootBlock(root)
	if err != nil {
		env.t.Fatalf("failed to insert root block #%d: %v", root.Number(), err)
	}
	if switched != wantSwitched {
		env.t.Fatalf("root block #%d switch mismatch: have %v, want %v", root.Number(), switched, wantSwitched)
	}
}

func (env *testEnv) checkRootTip(want *qkctypes.RootBlock) {
	env.t.Helper()
	if have := qkcrawdb.ReadRootHeadHash(env.db); have != want.Hash() {
		env.t.Fatalf("root tip mismatch: have [%x..], want #%d [%x..]", have.Bytes()[:4], want.Number(), want.Hash().Bytes()[:4])
	}
}

func (env *testEnv) checkLastConfirmed(root *qkctypes.RootBlock, want *types.Block) {
	env.t.Helper()
	if have := qkcrawdb.ReadLastConfirmedBlockHash(env.db, root.Hash()); have != want.Hash() {
		env.t.Fatalf("root block #%d last confirmed mismatch: have [%x..], want #%d [%x..]",
			root.Number(), have.Bytes()[:4], want.NumberU64(), want.Hash().Bytes()[:4])
	}
}

// makeBlocks creates n shard blocks on top of parent, referencing prevRoot.
// Blocks with different seeds are different.
func (env *testEnv) makeBlocks(parent *types.Block, n int, prevRoot *qkctypes.RootBlock, seed byte) []*types.Block {
	blocks, _ := core.GenerateChain(env.config, parent, env.engine, env.genDb, n, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{seed})
		b.SetExtra(qkcExtra(prevRoot))
	})
	return blocks
}

func (env *testEnv) insert(block *types.Block) {
	env.t.Helper()
	if err := env.sc.InsertShardBlock(block); err != nil {
		env.t.Fatalf("failed to insert block #%d: %v", block.NumberU64(), err)
	}
}

func (env *testEnv) checkHead(want *types.Block) {
	env.t.Helper()
	if head := env.chain.CurrentBlock(); head.Hash() != want.Hash() {
		env.t.Fatalf("head mismatch: have #%d [%x..], want #%d [%x..]",
			head.Number, head.Hash().Bytes()[:4], want.NumberU64(), want.Hash().Bytes()[:4])
	}
}

// The tests below come with diagrams in the style of the ones of ShardChain.
// They draw the root chain above the shard chain, with arrows from each block
// to its parent:
//
//   - r0, r1, ... are root blocks, and * marks the root tip. r1b is another
//     root block at the height of r1.
//   - g is the shard genesis. The other shard blocks are named by fork and
//     height: b2 is the block of fork b at height 2, b[1] in the code.
//   - x[r] is a shard block x referencing the root block r.
//   - A dotted arrow from a root block down to a shard block means the root
//     chain confirms the shard chain up to that block.

// TestInsertShardBlockExtendsHead checks that blocks extending the head become
// the head, and that known blocks are ignored. Fork a is blocks in the code.
//
//	root chain    r0*
//
//	shard chain   g[r0] <-- a1[r0] <-- a2[r0] <-- a3[r0]
//
//	a1, a2 and a3 become the head in turn. Inserting a1 again keeps a3.
func TestInsertShardBlockExtendsHead(t *testing.T) {
	env := newTestEnv(t)
	blocks := env.makeBlocks(env.genesis, 3, env.rootGenesis, 1)
	for _, block := range blocks {
		env.insert(block)
		env.checkHead(block)
	}
	// Known blocks are ignored.
	env.insert(blocks[0])
	env.checkHead(blocks[2])
}

// TestInsertShardBlockUnknownParent checks that a block whose parent is missing
// is rejected. Fork a is blocks in the code.
//
//	root chain    r0*
//
//	shard chain   g[r0] <-- (a1[r0]) <-- a2[r0]
//
//	a1 is not inserted, so inserting a2 fails.
func TestInsertShardBlockUnknownParent(t *testing.T) {
	env := newTestEnv(t)
	blocks := env.makeBlocks(env.genesis, 2, env.rootGenesis, 1)
	if err := env.sc.InsertShardBlock(blocks[1]); !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("error mismatch: have %v, want %v", err, consensus.ErrUnknownAncestor)
	}
}

// TestInsertShardBlockRootChainUninitialized checks that no block is accepted
// before the root tip is known. Fork a is blocks in the code.
//
//	root chain    r0              no root tip
//
//	shard chain   g[r0] <-- a1[r0]
//
//	Inserting a1 fails.
func TestInsertShardBlockRootChainUninitialized(t *testing.T) {
	env := newTestEnv(t)
	qkcrawdb.WriteRootHeadHash(env.db, common.Hash{})
	blocks := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	if err := env.sc.InsertShardBlock(blocks[0]); !errors.Is(err, ErrRootChainUninitialized) {
		t.Fatalf("error mismatch: have %v, want %v", err, ErrRootChainUninitialized)
	}
}

// TestInsertShardBlockRootReference checks the root reference rules of
// validateRootReference, on top of the chains below. Each invalid block x is
// rejected, and not inserted.
//
//	              +----- r1b
//	              v
//	root chain    r0 <-- r1 <-- r2* <-- r3
//	                            :       :
//	                            v       v
//	shard chain   g[r0] <------ a1[r1]  ?
//
//	r2 confirms a1, the head. r3 confirms ?, a block missing from the shard
//	chain.
func TestInsertShardBlockRootReference(t *testing.T) {
	env := newTestEnv(t)

	r0 := env.rootGenesis
	r1, r1b := newRoot(r0, 1), newRoot(r0, 2)
	r2 := newRoot(r1, 1)
	r3 := newRoot(r2, 1)
	a1 := env.makeBlocks(env.genesis, 1, r1, 1)[0]
	env.addRoot(r1, nil)
	env.addRoot(r1b, nil)
	env.addRoot(r2, a1)
	env.setRootTip(r2)
	env.insert(a1)
	env.checkHead(a1)

	qkcrawdb.WriteRootBlock(env.db, r3)
	qkcrawdb.WriteLastConfirmedBlockHash(env.db, r3.Hash(), common.Hash{0xff})

	unknown := newRoot(r1, 2)
	tests := []struct {
		name  string
		block *types.Block
		err   error
	}{
		// g[r0] <-- a1[r1] <-- x[u]
		//
		// u, a root block on top of r1, is not stored.
		{"unknown root", env.makeBlocks(a1, 1, unknown, 1)[0], ErrUnknownRootBlock},

		// g[r0] <-- a1[r1] <-- x[r0]
		//
		// r0 is older than r1.
		{"older root", env.makeBlocks(a1, 1, r0, 1)[0], ErrRootNotDescendant},

		// g[r0] <-- a1[r1] <-- x[r1b]
		//
		// r1b is on another root chain than r1.
		{"forked root", env.makeBlocks(a1, 1, r1b, 1)[0], ErrRootNotDescendant},

		// g[r0] <-- a1[r1]
		// ^
		// +-------- x[r2]
		//
		// r2 extends r0, but confirms a1, which x does not extend.
		{"not extending confirmed", env.makeBlocks(env.genesis, 1, r2, 2)[0], ErrNotConfirmedDescendant},

		// g[r0] <-- a1[r1] <-- x[r3]
		//
		// r3 extends r1, but confirms ?, which is missing from the shard chain.
		{"unknown confirmed", env.makeBlocks(a1, 1, r3, 1)[0], ErrUnknownConfirmedBlock},
	}
	for _, test := range tests {
		if err := env.sc.InsertShardBlock(test.block); !errors.Is(err, test.err) {
			t.Errorf("%s: error mismatch: have %v, want %v", test.name, err, test.err)
		}
		if env.chain.HasBlock(test.block.Hash(), test.block.NumberU64()) {
			t.Errorf("%s: invalid block inserted", test.name)
		}
	}
	env.checkHead(a1)

	// g[r0] <-- a1[r1] <-- a2[r2]
	//
	// a2 references r2, which extends r1, and a2 extends a1, which r2 confirms.
	// It is valid, and becomes the head.
	a2 := env.makeBlocks(a1, 1, r2, 1)[0]
	env.insert(a2)
	env.checkHead(a2)
}

// TestShouldUpdateHeadRootNotCanonical checks that a block referencing a root
// block off the root tip chain does not become the head (rule 1). Block a1 is
// block in the code.
//
//	              +----- r1b
//	              v
//	root chain    r0 <-- r1*
//
//	shard chain   g[r0] <-- a1[r1b]
//
//	a1 is valid, as r1b extends r0, so it is inserted. But r1b is not on the
//	chain of the root tip r1, so the head stays g.
func TestShouldUpdateHeadRootNotCanonical(t *testing.T) {
	env := newTestEnv(t)
	r1, r1b := newRoot(env.rootGenesis, 1), newRoot(env.rootGenesis, 2)
	env.addRoot(r1, nil)
	env.addRoot(r1b, nil)
	env.setRootTip(r1)

	block := env.makeBlocks(env.genesis, 1, r1b, 1)[0]
	env.insert(block)
	if !env.chain.HasBlock(block.Hash(), block.NumberU64()) {
		t.Fatal("block not inserted")
	}
	env.checkHead(env.genesis)
}

// TestShouldUpdateHeadLongerFork checks that a fork replaces the head once it
// is higher (rule 4).
//
//	root chain    r0*
//
//	shard chain   g[r0] <-- a1[r0]
//	              ^
//	              +-------- b1[r0] <-- b2[r0]
//
//	a1 becomes the head. b1, as high as a1 and referencing the same root
//	block, does not replace it. b2, higher, does.
func TestShouldUpdateHeadLongerFork(t *testing.T) {
	env := newTestEnv(t)
	a := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	b := env.makeBlocks(env.genesis, 2, env.rootGenesis, 2)

	env.insert(a[0])
	env.checkHead(a[0])

	// Same height and same root, the head stays.
	env.insert(b[0])
	env.checkHead(a[0])

	// Higher, the fork becomes the head.
	env.insert(b[1])
	env.checkHead(b[1])
}

// TestShouldUpdateHeadLaterRoot checks that, at the same height, a fork
// referencing a higher root block replaces the head (rule 4).
//
//	root chain    r0 <-- r1*
//
//	shard chain   g[r0] <-- a1[r0]
//	              ^
//	              +-------- b1[r1]
//
//	a1 becomes the head. b1, as high as a1 but referencing r1, higher than
//	r0, replaces it.
func TestShouldUpdateHeadLaterRoot(t *testing.T) {
	env := newTestEnv(t)
	r1 := newRoot(env.rootGenesis, 1)
	env.addRoot(r1, nil)
	env.setRootTip(r1)

	a := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	env.insert(a[0])
	env.checkHead(a[0])

	// Same height but a later root, the fork becomes the head.
	b := env.makeBlocks(env.genesis, 1, r1, 2)
	env.insert(b[0])
	env.checkHead(b[0])
}

// TestShouldUpdateHeadConfirmedBarrier checks that a fork not extending the
// block confirmed by the root tip never becomes the head (rule 3).
//
//	root chain    r0 <----- r1*
//	                        :
//	                        v
//	shard chain   g[r0] <-- a1[r0]
//	              ^
//	              +-------- b1[r0] <-- b2[r0] <-- b3[r0]
//
//	a1 becomes the head, then r1, confirming a1, becomes the root tip. None of
//	the blocks of fork b replaces a1, not even b2 and b3, higher than a1.
func TestShouldUpdateHeadConfirmedBarrier(t *testing.T) {
	env := newTestEnv(t)
	a := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	env.insert(a[0])

	r1 := newRoot(env.rootGenesis, 1)
	env.addRoot(r1, a[0])
	env.setRootTip(r1)

	b := env.makeBlocks(env.genesis, 3, env.rootGenesis, 2)
	for _, block := range b {
		env.insert(block)
		env.checkHead(a[0])
	}
}

// TestInsertRootBlockConfirms checks that root blocks extending the root tip
// become the root tip, record the latest shard block they confirm, and keep
// the head, and that known root blocks are ignored. Fork a is a in the code.
//
//	root chain    r0 <----- r1 <------ r2 <-- r3*
//	                        :          :
//	                        v          v
//	shard chain   g[r0] <-- a1[r0] <-- a2[r1] <-- a3[r1]
//
//	r1 confirms g and a1, r2 confirms a2, and r3 nothing more, so its last
//	confirmed block is a2 too. Inserting r1 again changes nothing.
func TestInsertRootBlockConfirms(t *testing.T) {
	env := newTestEnv(t)
	a := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	env.insert(a[0])

	r1 := newRoot(env.rootGenesis, 1, env.genesis.Header(), a[0].Header())
	env.insertRoot(r1, true)
	env.checkRootTip(r1)
	env.checkLastConfirmed(r1, a[0])
	env.checkHead(a[0])

	// Known root blocks are ignored.
	env.insertRoot(r1, false)
	env.checkRootTip(r1)

	a = append(a, env.makeBlocks(a[0], 2, r1, 1)...)
	env.insert(a[1])
	env.insert(a[2])

	r2 := newRoot(r1, 1, a[1].Header())
	env.insertRoot(r2, true)
	env.checkRootTip(r2)
	env.checkLastConfirmed(r2, a[1])
	env.checkHead(a[2])

	r3 := newRoot(r2, 1)
	env.insertRoot(r3, true)
	env.checkRootTip(r3)
	env.checkLastConfirmed(r3, a[1])
	env.checkHead(a[2])
}

// TestInsertRootBlockTotalDifficulty checks that a root block replaces the root
// tip only with a higher total difficulty, and is stored either way.
//
//	              +----- r1b, r1c*
//	              v
//	root chain    r0 <-- r1
//
//	r1 becomes the root tip. r1b, as heavy as r1, does not replace it, but
//	r1c, heavier, does.
func TestInsertRootBlockTotalDifficulty(t *testing.T) {
	env := newTestEnv(t)
	r1, r1b := newRoot(env.rootGenesis, 1), newRoot(env.rootGenesis, 2)
	env.insertRoot(r1, true)
	env.checkRootTip(r1)

	// Same total difficulty, the root tip stays.
	env.insertRoot(r1b, false)
	env.checkRootTip(r1)
	if !qkcrawdb.HasRootBlock(env.db, r1b.Hash()) {
		t.Fatal("root block not stored")
	}
	// Higher total difficulty, the root block becomes the root tip.
	seed := uint64(3)
	r1c := env.rootGenesis.Header().CreateBlockToAppend(nil, uint256.NewInt(2), nil, &seed, nil)
	env.insertRoot(r1c, true)
	env.checkRootTip(r1c)
	env.checkHead(env.genesis)
}

// TestInsertRootBlockValidation checks the rules of validateRootBlock, on top
// of the chains below. Each invalid root block x is rejected, and not stored.
// Fork a is a in the code, and a root block carries at most one block of the
// shard.
//
//	              +----- r1b
//	              v
//	root chain    r0 <----- r1*
//	                        :
//	                        v
//	shard chain   g[r0] <-- a1[r0] <-- a2[r0] <-- a3[r0]
//	                        ^
//	                        +-------- b2[r1b]
//
//	r1 confirms g and a1. The other shard 1 has blocks s1 to s4: s1 and s2
//	reference r1, s3 and s4 reference r0. The deposits of s1 and s4 are
//	stored. Shard 7, not in the network, has a block t1 referencing r1,
//	whose deposits are stored.
func TestInsertRootBlockValidation(t *testing.T) {
	env := newTestEnv(t)
	r0 := env.rootGenesis
	a := env.makeBlocks(env.genesis, 3, r0, 1)
	for _, block := range a {
		env.insert(block)
	}
	r1, r1b := newRoot(r0, 1, env.genesis.Header(), a[0].Header()), newRoot(r0, 2)
	env.insertRoot(r1, true)
	env.insertRoot(r1b, false)
	b2 := env.makeBlocks(a[0], 1, r1b, 2)[0]
	env.insert(b2)

	s1, s2, s3, s4 := remoteHeader(1, 1, r1), remoteHeader(1, 2, r1), remoteHeader(1, 3, r0), remoteHeader(1, 4, r0)
	t1 := remoteHeader(7, 1, r1)
	env.storeXShardDeposits(s1)
	env.storeXShardDeposits(s4)
	env.storeXShardDeposits(t1)
	s3Extra, err := s3.QKCExtra()
	if err != nil {
		t.Fatal(err)
	}
	if err := env.sc.validateXShardDeposits(s3, s3Extra, r1.Header(), env.config.QuarkChain); err != nil {
		t.Fatalf("block referencing a root before the shard genesis root should not require deposits: %v", err)
	}
	env.chain.Config().QuarkChain.MaxBlocksPerRootBlock = 1

	badVersion := newRoot(r1, 1).Header()
	badVersion.Version = 1
	tests := []struct {
		name string
		root *qkctypes.RootBlock
		err  error
	}{
		// Rule 1.
		{"unknown parent", newRoot(newRoot(r1, 2), 1), ErrUnknownRootBlock},
		{"genesis height", qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{ParentHash: r0.Hash()}), ErrRootNotAfterGenesis},
		{"version", qkctypes.NewRootBlockWithHeader(badVersion), ErrRootBlockVersion},

		// Rule 2: x carries a2 of a fork missing from the shard chain, then
		// a2 and a3, more than one block of the shard.
		{"unknown block", newRoot(r1, 1, env.makeBlocks(a[0], 1, r1, 3)[0].Header()), ErrUnknownConfirmedBlock},
		{"too many blocks", newRoot(r1, 1, a[1].Header(), a[2].Header()), ErrTooManyConfirmedBlocks},

		// Rule 2: x carries s2, missing its deposits, then s4 and t1, which
		// send none but come with some.
		{"missing deposits", newRoot(r1, 1, s2), ErrMissingXShardDeposits},
		{"deposits before genesis root", newRoot(r1, 1, s4), ErrUnexpectedXShardDeposits},
		{"deposits from unknown shard", newRoot(r1, 1, t1), ErrUnexpectedXShardDeposits},

		// Rule 3: x carries a3, which is not a child of a1, then b2, which
		// is, but references r1b, not an ancestor of x.
		{"gap", newRoot(r1, 1, a[2].Header()), ErrConfirmedNotContiguous},
		{"regression to genesis", newRoot(r1, 1, env.genesis.Header()), ErrConfirmedNotContiguous},
		{"confirmed root fork", newRoot(r1, 1, b2.Header()), ErrConfirmedRootNotAncestor},
	}
	for _, test := range tests {
		if _, err := env.sc.InsertRootBlock(test.root); !errors.Is(err, test.err) {
			t.Errorf("%s: error mismatch: have %v, want %v", test.name, err, test.err)
		}
		if qkcrawdb.HasRootBlock(env.db, test.root.Hash()) {
			t.Errorf("%s: invalid root block stored", test.name)
		}
	}
	env.checkRootTip(r1)
	env.checkHead(a[2])

	// r2 carries a2, s1 and s3. a2 is a child of a1, and references r0. s1
	// comes with its deposits, and s3, referencing r0, sends none. It is
	// valid, and becomes the root tip.
	r2 := newRoot(r1, 1, a[1].Header(), s1, s3)
	env.insertRoot(r2, true)
	env.checkRootTip(r2)
	env.checkLastConfirmed(r2, a[1])
	env.checkHead(a[2])
}

// TestInsertRootBlockConfirmsFork checks that the head moves to the latest
// block confirmed by the new root tip when it is on a fork of the head (case 1
// of setHeadForRootTip).
//
//	root chain    r0 <----- r1 <------ r2*
//	                        :          :
//	                        v          v
//	shard chain   g[r0] <-- a1[r0] <-- b2[r0] <-- b3[r0]
//	                        ^
//	                        +-------- a2[r0] <-- a3[r0]
//
//	a3 is the head. r1 confirms g and a1, and keeps it. r2 confirms b2, so
//	the head moves to b2, not to b3.
func TestInsertRootBlockConfirmsFork(t *testing.T) {
	env := newTestEnv(t)
	a := env.makeBlocks(env.genesis, 3, env.rootGenesis, 1)
	b := env.makeBlocks(a[0], 2, env.rootGenesis, 2)
	for _, block := range append(a, b...) {
		env.insert(block)
	}
	env.checkHead(a[2])

	r1 := newRoot(env.rootGenesis, 1, env.genesis.Header(), a[0].Header())
	env.insertRoot(r1, true)
	env.checkHead(a[2])

	r2 := newRoot(r1, 1, b[0].Header())
	env.insertRoot(r2, true)
	env.checkRootTip(r2)
	env.checkHead(b[0])
}

// TestInsertRootBlockConfirmsAboveHead checks that the head moves forward to
// the latest block confirmed by the new root tip when it extends the head (case
// 1 of setHeadForRootTip).
//
//	              +----- r1b <-- r2b*
//	              v              :
//	root chain    r0 <-- r1      :
//	                             v
//	shard chain   g[r0] <------- a1[r1b]
//
//	r1 is the root tip, so a1, referencing r1b, does not become the head.
//	r2b, heavier than r1, confirms g and a1, so the head moves to a1.
func TestInsertRootBlockConfirmsAboveHead(t *testing.T) {
	env := newTestEnv(t)
	r1, r1b := newRoot(env.rootGenesis, 1), newRoot(env.rootGenesis, 2)
	env.insertRoot(r1, true)
	env.insertRoot(r1b, false)

	a := env.makeBlocks(env.genesis, 1, r1b, 1)
	env.insert(a[0])
	env.checkHead(env.genesis)

	r2b := newRoot(r1b, 1, env.genesis.Header(), a[0].Header())
	env.insertRoot(r2b, true)
	env.checkRootTip(r2b)
	env.checkHead(a[0])
}

// TestInsertRootBlockRewindsHead checks that the head moves back to the latest
// block referencing the new root tip or one of its ancestors (case 2 of
// setHeadForRootTip). Fork a is a in the code.
//
//	                        +----- r2b
//	                        v
//	root chain    r0 <----- r1 <-- r2 <-- r3*
//	                        :
//	                        v
//	shard chain   g[r0] <-- a1[r0] <-- a2[r1] <-- a3[r2b] <-- a4[r2b]
//
//	r1 confirms g and a1, and so do its descendants. r2b becomes the root
//	tip, and a2 to a4 the head in turn. r2, as heavy as r2b, does not
//	replace it, but r3 does, so the head moves back from a4 to a2.
func TestInsertRootBlockRewindsHead(t *testing.T) {
	env := newTestEnv(t)
	a := env.makeBlocks(env.genesis, 1, env.rootGenesis, 1)
	env.insert(a[0])

	r1 := newRoot(env.rootGenesis, 1, env.genesis.Header(), a[0].Header())
	r2, r2b := newRoot(r1, 1), newRoot(r1, 2)
	env.insertRoot(r1, true)
	env.insertRoot(r2b, true)

	a = append(a, env.makeBlocks(a[0], 1, r1, 1)...)
	a = append(a, env.makeBlocks(a[1], 2, r2b, 1)...)
	for _, block := range a[1:] {
		env.insert(block)
	}
	env.checkHead(a[3])

	env.insertRoot(r2, false)
	env.checkHead(a[3])

	r3 := newRoot(r2, 1)
	env.insertRoot(r3, true)
	env.checkRootTip(r3)
	env.checkLastConfirmed(r3, a[0])
	env.checkHead(a[1])
}
