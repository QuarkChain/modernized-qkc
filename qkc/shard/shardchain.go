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

// Package shard implements the consensus layer of a QuarkChain shard.
package shard

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

var (
	// ErrRootChainUninitialized is returned when no root tip is known yet.
	ErrRootChainUninitialized = errors.New("root chain not initialized")

	// ErrUnknownRootBlock is returned when a shard block references a root block
	// that is not known yet.
	ErrUnknownRootBlock = errors.New("unknown root block")

	// ErrRootNotDescendant is returned when the root block referenced by a shard
	// block does not extend the one referenced by its parent.
	ErrRootNotDescendant = errors.New("previous root block does not extend the one of the parent")

	// ErrNotConfirmedDescendant is returned when a shard block does not extend
	// the latest shard block confirmed by the root block it references.
	ErrNotConfirmedDescendant = errors.New("block does not extend the last confirmed block")

	// ErrUnknownConfirmedBlock is returned when the root chain confirms a shard
	// block that is not in the local chain.
	ErrUnknownConfirmedBlock = errors.New("unknown confirmed block")

	// ErrRootNotAfterGenesis is returned when a root block is not higher than
	// the root block referenced by the shard genesis.
	ErrRootNotAfterGenesis = errors.New("root block not after the root block of the shard genesis")

	// ErrRootBlockVersion is returned when a root block has an unsupported
	// version.
	ErrRootBlockVersion = errors.New("unsupported root block version")

	// ErrTooManyConfirmedBlocks is returned when a root block confirms more
	// blocks of the shard than MaxBlocksPerRootBlock.
	ErrTooManyConfirmedBlocks = errors.New("too many shard blocks in the root block")

	// ErrConfirmedNotContiguous is returned when the first block of the shard
	// confirmed by a root block does not follow the latest one confirmed by its
	// parent.
	ErrConfirmedNotContiguous = errors.New("confirmed blocks do not follow the last confirmed block")

	// ErrConfirmedRootNotAncestor is returned when the latest shard block
	// confirmed by a root block references a root block that is not one of its
	// ancestors.
	ErrConfirmedRootNotAncestor = errors.New("last confirmed block references a root block off the root chain")

	// ErrMissingXShardDeposits is returned when a root block confirms a block of
	// another shard whose cross-shard deposits to this shard are not stored.
	ErrMissingXShardDeposits = errors.New("missing cross-shard deposits")

	// ErrUnexpectedXShardDeposits is returned when cross-shard deposits are
	// stored for a block of another shard that sends none to this shard.
	ErrUnexpectedXShardDeposits = errors.New("unexpected cross-shard deposits")

	errNotQuarkChain = errors.New("not a QuarkChain shard chain")
)

// ShardChain is the consensus layer of a shard. It imports shard blocks into
// the underlying chain under the root chain rules, imports the root blocks, and
// selects the canonical shard head.
type ShardChain struct {
	db    ethdb.KeyValueStore // Database of the root blocks seen by the shard
	chain *core.BlockChain

	mu sync.Mutex // Serializes block imports and head updates
}

// NewShardChain creates a shard chain on top of the given chain, reading the
// root blocks from db.
func NewShardChain(db ethdb.KeyValueStore, chain *core.BlockChain) *ShardChain {
	return &ShardChain{db: db, chain: chain}
}

// InsertShardBlock validates a block against the root chain, imports it, and
// makes it the canonical head if the fork choice rule selects it. Known blocks
// are ignored.
func (c *ShardChain) InsertShardBlock(block *types.Block) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO 1. check shard id here 2. check future block in engine.VerifyBlockHeader

	if c.chain.HasBlock(block.Hash(), block.NumberU64()) {
		return nil
	}
	rootTip := c.rootTip()
	if rootTip == nil {
		return ErrRootChainUninitialized
	}
	parent := c.chain.GetHeader(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	prevRoot, err := c.validateRootReference(block.Header(), parent)
	if err != nil {
		return err
	}

	// post cross shard execution will be validated in InsertBlockWithoutSetHead
	if _, err := c.chain.InsertBlockWithoutSetHead(context.Background(), block, false); err != nil {
		return err
	}
	if c.shouldUpdateHead(block.Header(), prevRoot, rootTip) {
		if _, err := c.chain.SetCanonical(block); err != nil {
			return fmt.Errorf("set canonical head %x: %w", block.Hash(), err)
		}
	}
	return nil
}

// validateRootReference checks the root block referenced by a shard block, its
// previous root block, against the shard and root chains, and returns it. It
// mirrors the root checks of ShardState.validate_block in pyquarkchain, whose
// error messages are quoted below. Two rules apply.
//
// Rule 1: the previous root block of the block must be the one of its parent,
// or a descendant of it ("prev root block height must be non-decreasing",
// "prev root blocks are not on the same chain"). The view a shard chain has of
// the root chain only moves forward, along a single root chain. The
// cross-shard cursor relies on it: a block applies the deposits confirmed by
// the root blocks from the previous root block of its parent up to its own.
//
//	              +----- R1'
//	              v
//	root chain    R0 <-- R1 <-- R2 <-- R3
//	                     ^
//	                     |
//	shard chain   ... <- P <--- B
//
//	P references R1, so B may reference R1, R2 or R3, but neither R0, which is
//	older, nor R1', which is on another root chain.
//
// A fork after R1 passes this rule, as it still extends R1. Whether the block
// then becomes the head is up to the fork choice, see shouldUpdateHead.
//
// Rule 2: the block must extend the latest shard block confirmed by the root
// chain up to its previous root block ("prev root block's minor block is not in
// the same chain as the minor block"). Once a root block confirms a shard
// block, the shard blocks that have seen that root block cannot fork away from
// it.
//
//	root chain    R0 <-- R1
//	                     :  confirms up to C
//	                     v
//	shard chain   G <--- C <-- P <-- B     B extends C: valid
//	              ^
//	              +--- P' <-- B'           B' forks before C: invalid
//
//	B and B' both reference R1.
func (c *ShardChain) validateRootReference(header, parent *types.Header) (*qkctypes.RootBlockHeader, error) {
	// Both root blocks must be known ("cannot find root block for the minor
	// block"). The one of the parent was already checked on its import.
	prevRoot, err := c.prevRootBlock(header)
	if err != nil {
		return nil, err
	}
	parentPrevRoot, err := c.prevRootBlock(parent)
	if err != nil {
		return nil, fmt.Errorf("parent: %w", err)
	}
	// Rule 1. pyquarkchain checks the heights first, which is implied here, as
	// a root block is never lower than its ancestors.
	if !c.isRootDescendant(prevRoot, parentPrevRoot) {
		return nil, fmt.Errorf("%w: root #%d [%x..] after #%d [%x..]", ErrRootNotDescendant,
			prevRoot.Number, prevRoot.Hash().Bytes()[:4], parentPrevRoot.Number, parentPrevRoot.Hash().Bytes()[:4])
	}
	// Rule 2, skipped while the root chain confirms no block of the shard yet,
	// as after genesis.
	confirmed, err := c.lastConfirmed(prevRoot)
	if err != nil {
		return nil, err
	}
	// The block itself is not stored yet, so check its parent instead: the
	// block extends C if and only if its parent is C or extends it.
	if confirmed != nil && !c.isShardDescendant(parent, confirmed) {
		return nil, fmt.Errorf("%w: confirmed #%d [%x..]", ErrNotConfirmedDescendant,
			confirmed.Number, confirmed.Hash().Bytes()[:4])
	}
	return prevRoot, nil
}

// shouldUpdateHead reports whether a newly imported shard block, referencing
// the root block prevRoot, should become the canonical head. It mirrors the tip
// update of ShardState.add_block in pyquarkchain: "Update tip if a block is
// appended or a fork is longer (with the same ancestor confirmed by root block
// tip) or they are equal length but the root height confirmed by the block is
// longer". The rules apply in order:
//
//  1. The block must reference the root tip or one of its ancestors. A block
//     referencing another root chain, like R1' below, is never selected.
//  2. A block extending the head is selected.
//  3. Otherwise the block is on a fork, which must extend C, the latest shard
//     block confirmed by the root tip.
//  4. The higher of the fork and the head wins. At the same height, the fork
//     wins if it references a higher root block than the head, as it has seen
//     more of the root chain.
//
// For example, with R2 as the root tip and H as the head:
//
//	              +----- R1'
//	              v
//	root chain    R0 <-- R1 <-- R2
//	                            :  confirms up to C
//	                            v
//	shard chain   G <-- ... <-- C <-- H
//	              ^             ^
//	              |             +---- X <-- Y
//	              +---- F <-- ... <-- Z
//
//	A child of H is selected (rule 2). Y, higher than H, is selected, and so
//	is X, as high as H, if it references a higher root block than H does
//	(rule 4). Z is never selected, however high, as its fork does not extend
//	C (rule 3).
//
// As in pyquarkchain, forks are compared by height, not by difficulty.
func (c *ShardChain) shouldUpdateHead(header *types.Header, prevRoot, rootTip *qkctypes.RootBlockHeader) bool {
	// Rule 1: "Don't update tip if the block depends on a root block that is
	// not root_tip or root_tip's ancestor".
	if !c.isRootDescendant(rootTip, prevRoot) {
		return false
	}
	// Rule 2. The head always extends C, so the block does too. The root tip
	// update must keep it so, by moving the head onto the shard chain confirmed
	// by the new root tip.
	current := c.chain.CurrentBlock()
	if header.ParentHash == current.Hash() {
		return true
	}
	// Rule 3, as __is_minor_block_linked_to_root_tip: "Determine whether a
	// minor block is a descendant of a minor block confirmed by root tip". Any
	// fork qualifies while the root chain confirms no block of the shard yet,
	// as after genesis. pyquarkchain first checks that the block is higher than
	// C, which is implied here: the block is new, so it is not C, and it only
	// extends C if it is higher.
	confirmed, err := c.lastConfirmed(rootTip)
	if err != nil {
		return false
	}
	if confirmed != nil && !c.isShardDescendant(header, confirmed) {
		return false
	}
	// Rule 4. On a full tie, the head stays.
	if header.Number.Uint64() != current.Number.Uint64() {
		return header.Number.Uint64() > current.Number.Uint64()
	}

	// If both shard headers has the same block number,
	// compare the view of the root block and switch to the minor
	// block with longer root block for more cross shard tx fee.
	currentRoot, err := c.prevRootBlock(current)
	return err == nil && prevRoot.Number > currentRoot.Number
}

// InsertRootBlock validates a root block against the shard chain, stores it,
// and makes it the root tip if the fork choice rule selects it, moving the
// canonical head onto the shard chain it confirms. It reports whether the root
// tip changed. Known root blocks are ignored. It mirrors
// ShardState.add_root_block in pyquarkchain.
//
// Unlike shard blocks, root blocks are chosen by total difficulty: "Switch to
// the root block with higher total diff". On a tie, the root tip stays.
func (c *ShardChain) InsertRootBlock(root *qkctypes.RootBlock) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A known root block was handled by an earlier call: it became the root tip
	// if it had a higher total difficulty, and the root tip only gains total
	// difficulty since, so inserting it again changes nothing.
	if rawdb.HasRootBlock(c.db, root.Hash()) {
		return false, nil
	}
	rootTip := c.rootTip()
	if rootTip == nil {
		return false, ErrRootChainUninitialized
	}
	// Step 1: validate the root block, and find C, the latest shard block
	// confirmed by the root chain up to it.
	confirmed, err := c.validateRootBlock(root)
	if err != nil {
		return false, err
	}
	// Step 2: the fork choice.
	switched := root.TotalDifficulty().Cmp(&rootTip.TotalDifficulty) > 0

	// Step 3: on a switch, move the head onto the shard chain of the new root
	// tip. This comes before the root block is stored: if storing it fails, the
	// root block stays unknown, and inserting it again redoes the switch. In the
	// other order, the root tip could change without the head, and inserting
	// the root block again would not fix it, as it is known.
	if switched {
		if err := c.setHeadForRootTip(root.Header(), confirmed); err != nil {
			return false, err
		}
	}
	// Step 4: store the root block, C and the root tip at once. C is stored per
	// root block, so the one of the root tip, confirmed_header_tip in
	// pyquarkchain, moves along with the root tip.
	batch := c.db.NewBatch()
	defer batch.Close()
	rawdb.WriteRootBlock(batch, root)
	if confirmed != nil {
		rawdb.WriteLastConfirmedBlockHash(batch, root.Hash(), confirmed.Hash())
	}
	if switched {
		rawdb.WriteRootHeadHash(batch, root.Hash())
	}
	if err := batch.Write(); err != nil {
		return false, fmt.Errorf("store root block %x: %w", root.Hash(), err)
	}
	return switched, nil
}

// validateRootBlock checks a root block against the shard and root chains, and
// returns C, the latest shard block confirmed by the root chain up to it, or nil
// if none is confirmed yet. It mirrors the checks of ShardState.add_root_block
// in pyquarkchain, whose error messages are quoted below. As there, the rest of the root block, such as its proof of work, its
// total difficulty or the merkle root of the shard block headers it carries, is
// left to the root chain. Three rules apply.
//
// Rule 1: the root block must extend a known root block, and be higher than the
// root block referenced by the shard genesis ("cannot find previous root block
// in pool"). The shard knows no root block below that one, so the root block of
// the shard genesis is an ancestor of all the others.
//
// Rule 2: the shard block headers it carries must be known to the shard. The
// blocks of this shard must be in the local chain ("cannot find minor block in
// local shard"), and at most MaxBlocksPerRootBlock ("too many minor blocks in
// the root block"). The blocks of the other shards must come with the
// cross-shard deposits they send to this shard ("cannot find x_shard tx
// list"), see validateXShardDeposits.
//
// Rule 3: the blocks of this shard it carries must extend C', the latest one
// confirmed by its parent, and C, the last of them, or C' if there are none,
// must reference one of its ancestors. As in pyquarkchain, only the first block
// is checked against C', the order of the others is left to the root chain
// ("Master should assure this check will not fail").
//
//	root chain    R0 <-- R1 <------------ R
//	                     :                :
//	                     v                v
//	shard chain   G <--- C' <-- A <------ C
//
//	R1 confirms the shard chain up to C', and R, on top of R1, up to C, so
//	R carries A and C. A must be a child of C', and C must reference R1 or
//	an ancestor of it.
func (c *ShardChain) validateRootBlock(root *qkctypes.RootBlock) (*types.Header, error) {
	config := c.chain.Config().QuarkChain
	if config == nil {
		return nil, errNotQuarkChain
	}
	header := root.Header()
	// "incorrect root block version"
	if header.Version != 0 {
		return nil, fmt.Errorf("%w: %d", ErrRootBlockVersion, header.Version)
	}
	// Rule 1: the root block is higher than the root block of the shard
	// genesis, which is known as the shard chain starts on it, and its parent
	// is known.
	genesisRoot, err := c.prevRootBlock(c.chain.Genesis().Header())
	if err != nil {
		return nil, fmt.Errorf("genesis: %w", err)
	}
	if header.Number <= genesisRoot.Number {
		return nil, fmt.Errorf("%w: root #%d, genesis root #%d", ErrRootNotAfterGenesis, header.Number, genesisRoot.Number)
	}
	parent := rawdb.ReadRootBlockHeader(c.db, header.ParentHash)
	if parent == nil {
		return nil, fmt.Errorf("%w: parent %x", ErrUnknownRootBlock, header.ParentHash)
	}
	// Rule 2: the blocks of this shard are in the local chain, at most
	// MaxBlocksPerRootBlock of them, and the blocks of the other shards come
	// with their cross-shard deposits. The blocks of this shard are kept in
	// their order for rule 3.
	var blocks []*types.Header
	for _, block := range root.MinorBlockHeaders() {
		extra, err := block.QKCExtra()
		if err != nil {
			return nil, fmt.Errorf("shard block %x: %w", block.Hash(), err)
		}
		if extra.ShardID != config.ShardID {
			if err := c.validateXShardDeposits(block, extra, genesisRoot, config); err != nil {
				return nil, err
			}
			continue
		}
		if !c.chain.HasBlock(block.Hash(), block.Number.Uint64()) {
			return nil, fmt.Errorf("%w: #%d [%x..]", ErrUnknownConfirmedBlock, block.Number, block.Hash().Bytes()[:4])
		}
		blocks = append(blocks, block)
	}
	if limit := config.MaxBlocksPerRootBlock; limit != 0 && uint64(len(blocks)) > limit {
		return nil, fmt.Errorf("%w: %d, max %d", ErrTooManyConfirmedBlocks, len(blocks), limit)
	}
	// Rule 3: the first block of this shard is a child of C', the latest one
	// confirmed by the parent, or the shard genesis, which the root chain
	// confirms first.
	confirmed, err := c.lastConfirmed(parent)
	if err != nil {
		return nil, err
	}
	if len(blocks) > 0 {
		first := blocks[0]
		if first.Number.Uint64() != 0 && (confirmed == nil || first.ParentHash != confirmed.Hash()) {
			return nil, fmt.Errorf("%w: first #%d [%x..]", ErrConfirmedNotContiguous, first.Number, first.Hash().Bytes()[:4])
		}
		confirmed = blocks[len(blocks)-1]
	}
	// Rule 3: C, the last block of this shard, or C' if there are none,
	// references an ancestor of the root block.
	if confirmed != nil {
		prevRoot, err := c.prevRootBlock(confirmed)
		if err != nil {
			return nil, err
		}
		if !c.isRootDescendant(header, prevRoot) {
			return nil, fmt.Errorf("%w: confirmed #%d [%x..] references root #%d [%x..]", ErrConfirmedRootNotAncestor,
				confirmed.Number, confirmed.Hash().Bytes()[:4], prevRoot.Number, prevRoot.Hash().Bytes()[:4])
		}
	}
	return confirmed, nil
}

// validateXShardDeposits checks that the cross-shard deposits sent to this shard
// by a block of another shard, carried by a root block, are stored if and only
// if the block sends any, as in ShardState.add_root_block in pyquarkchain. A
// block sends deposits, possibly an empty list, once cross-shard transactions
// are enabled, after the root block of the shard genesis, and if it comes from
// a shard of the network: each shard sends to all the others, which stands for
// the neighbor rule of pyquarkchain. A block that sends none must come with
// none ("shouldn't have been broadcasted").
func (c *ShardChain) validateXShardDeposits(block *types.Header, extra *types.QKCExtra, genesisRoot *qkctypes.RootBlockHeader, config *params.QuarkChainConfig) error {
	// The root block referenced by the block is unknown if it is older than
	// the root block of the shard genesis: "prev_root_header can be None when
	// the shard is not created at root height 0".
	prevRoot := rawdb.ReadRootBlockHeader(c.db, extra.PrevRootBlock)
	sends := prevRoot != nil && prevRoot.Number != genesisRoot.Number && config.IsXShardDestination(extra.ShardID)
	stored := rawdb.HasXShardDeposits(c.db, block.Hash())
	switch {
	case sends && !stored:
		return fmt.Errorf("%w: shard %d block #%d [%x..]", ErrMissingXShardDeposits, extra.ShardID, block.Number, block.Hash().Bytes()[:4])
	case !sends && stored:
		return fmt.Errorf("%w: shard %d block #%d [%x..]", ErrUnexpectedXShardDeposits, extra.ShardID, block.Number, block.Hash().Bytes()[:4])
	}
	return nil
}

// setHeadForRootTip moves the canonical head onto the shard chain of a new root
// tip, given C, the latest shard block confirmed by the root chain up to it, or
// nil if none. It mirrors the tip reset of ShardState.add_root_block in
// pyquarkchain. The head must keep the two properties shouldUpdateHead relies
// on: it extends C, and it references the root tip or one of its ancestors. Two
// cases apply.
//
// Case 1: C is not on the canonical chain, as the root tip confirms another
// fork, or blocks above the head. The head moves to C, which has both
// properties, see rule 3 of validateRootBlock. As in pyquarkchain, it stays on
// C even if blocks extend C ("shard_header might not be the tip of the longest
// chain").
//
//	root chain    R0 <-- R1 <-- R2
//	                     :      :
//	                     v      v
//	shard chain   G <--- A <--- C <--- D
//	                     ^
//	                     +----- X <--- H
//
//	R1 confirms the shard chain up to A, and the new root tip R2 up to C.
//	C is on a fork of the head H, so the head moves from H to C, not to D.
//
// Case 2: C is canonical, or there is none. The head already extends C, but
// may reference a root block on a fork of the root tip. The head moves back to
// the latest block referencing the root tip or one of its ancestors, C at the
// latest: "we need to scan back until finding a minor block pointing to the
// same root chain r_block is on".
//
//	                     +----- R2'
//	                     v
//	root chain    R0 <-- R1 <-- R2 <-- R3
//	                     :
//	                     v
//	shard chain   G <--- C <--- X <--- Y <--- H
//
//	R1 and its descendants confirm the shard chain up to C. X references
//	R1, and Y and H reference R2', the former root tip. With R3 as the new
//	root tip, the head moves back from H to X.
func (c *ShardChain) setHeadForRootTip(rootTip *qkctypes.RootBlockHeader, confirmed *types.Header) error {
	current := c.chain.CurrentBlock()
	head := current
	// Case 1.
	if confirmed != nil && c.chain.GetCanonicalHash(confirmed.Number.Uint64()) != confirmed.Hash() {
		head = confirmed
	}
	// Case 2, which keeps C in case 1. The walk stops at the shard genesis at
	// the latest, as it references the root block of the shard genesis, an
	// ancestor of every root block above it (rule 1 of validateRootBlock).
	// pyquarkchain resets the shard genesis there instead, as a shard created
	// after the root genesis has a genesis block per root chain, while the one
	// here is fixed.
	for {
		prevRoot, err := c.prevRootBlock(head)
		if err != nil {
			return err
		}
		if c.isRootDescendant(rootTip, prevRoot) {
			break
		}
		if head.Number.Uint64() == 0 {
			return fmt.Errorf("%w: genesis references root #%d [%x..]", ErrRootNotDescendant, prevRoot.Number, prevRoot.Hash().Bytes()[:4])
		}
		if head = c.chain.GetHeader(head.ParentHash, head.Number.Uint64()-1); head == nil {
			return consensus.ErrUnknownAncestor
		}
	}
	if head.Hash() == current.Hash() {
		return nil
	}
	block := c.chain.GetBlock(head.Hash(), head.Number.Uint64())
	if block == nil {
		return fmt.Errorf("%w: head #%d [%x..]", consensus.ErrUnknownAncestor, head.Number, head.Hash().Bytes()[:4])
	}
	if _, err := c.chain.SetCanonical(block); err != nil {
		return fmt.Errorf("set canonical head %x: %w", head.Hash(), err)
	}
	return nil
}

// rootTip returns the header of the root tip, or nil if it is not known.
func (c *ShardChain) rootTip() *qkctypes.RootBlockHeader {
	hash := rawdb.ReadRootHeadHash(c.db)
	if hash == (common.Hash{}) {
		return nil
	}
	return rawdb.ReadRootBlockHeader(c.db, hash)
}

// prevRootBlock returns the header of the root block referenced by a shard
// block.
func (c *ShardChain) prevRootBlock(header *types.Header) (*qkctypes.RootBlockHeader, error) {
	extra, err := header.QKCExtra()
	if err != nil {
		return nil, err
	}
	root := rawdb.ReadRootBlockHeader(c.db, extra.PrevRootBlock)
	if root == nil {
		return nil, fmt.Errorf("%w: %x", ErrUnknownRootBlock, extra.PrevRootBlock)
	}
	return root, nil
}

// lastConfirmed returns the header of the latest shard block confirmed by the
// root chain up to the given root block, or nil if none is confirmed yet.
func (c *ShardChain) lastConfirmed(root *qkctypes.RootBlockHeader) (*types.Header, error) {
	hash := rawdb.ReadLastConfirmedBlockHash(c.db, root.Hash())
	if hash == (common.Hash{}) {
		return nil, nil
	}
	header := c.chain.GetHeaderByHash(hash)
	if header == nil {
		// Unexpected error: InsertRootBlock only stores root blocks whose confirmed blocks are in the local chain.
		// TODO: the shard may be stale, and upper layer should handle this error, likely resync the cluster
		return nil, fmt.Errorf("%w: %x", ErrUnknownConfirmedBlock, hash)
	}
	return header, nil
}

// isRootDescendant reports whether the root block descendant extends, or is,
// the root block ancestor.
func (c *ShardChain) isRootDescendant(descendant, ancestor *qkctypes.RootBlockHeader) bool {
	for descendant.Number > ancestor.Number {
		if descendant = rawdb.ReadRootBlockHeader(c.db, descendant.ParentHash); descendant == nil {
			return false
		}
	}
	return descendant.Hash() == ancestor.Hash()
}

// isShardDescendant reports whether the shard block descendant extends, or is,
// the shard block ancestor.
func (c *ShardChain) isShardDescendant(descendant, ancestor *types.Header) bool {
	for descendant.Number.Uint64() > ancestor.Number.Uint64() {
		if descendant = c.chain.GetHeader(descendant.ParentHash, descendant.Number.Uint64()-1); descendant == nil {
			return false
		}
	}
	return descendant.Hash() == ancestor.Hash()
}
