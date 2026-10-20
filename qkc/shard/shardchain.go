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
	// clusterInconsistencyErr marks failures of guarantees that other cluster
	// components must establish before calling ShardChain. These defensive
	// checks should never fail here; a failure means an upstream guarantee was
	// violated, corresponding to an explicit check assertion in pyquarkchain.
	// Keep the specific cause wrapped alongside it for diagnosis in logs.
	clusterInconsistencyErr = errors.New("cluster inconsistency")

	// shardInconsistencyErr marks missing or inconsistent local data that was
	// already validated when stored. Incoming validation errors do not use it.
	shardInconsistencyErr = errors.New("shard inconsistency")

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

	// ErrRootNotAfterGenesis is returned when the root block's number is not
	// greater than the number of the root block referenced by the shard genesis.
	ErrRootNotAfterGenesis = errors.New("root block number must be greater than the root block referenced by the shard genesis")

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

	updateHead, err := c.shouldUpdateHead(block.Header(), prevRoot, rootTip)
	if err != nil {
		return err
	}
	// post cross shard execution will be validated in InsertBlockWithoutSetHead
	if _, err := c.chain.InsertBlockWithoutSetHead(context.Background(), block, false); err != nil {
		return err
	}
	if updateHead {
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
		return nil, fmt.Errorf("%w: parent: %w", shardInconsistencyErr, err)
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

// shouldUpdateHead reports whether a new shard block, referencing
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
// As in pyquarkchain, forks are compared by height, not by difficulty. Errors
// reading local chain data are returned instead of silently rejecting the fork.
func (c *ShardChain) shouldUpdateHead(header *types.Header, prevRoot, rootTip *qkctypes.RootBlockHeader) (bool, error) {
	// Rule 1: "Don't update tip if the block depends on a root block that is
	// not root_tip or root_tip's ancestor".
	if !c.isRootDescendant(rootTip, prevRoot) {
		return false, nil
	}
	// Rule 2. The head always extends C, so the block does too. The root tip
	// update must keep it so, by moving the head onto the shard chain confirmed
	// by the new root tip.
	current := c.chain.CurrentBlock()
	if header.ParentHash == current.Hash() {
		return true, nil
	}
	// Rule 3, as __is_minor_block_linked_to_root_tip: "Determine whether a
	// minor block is a descendant of a minor block confirmed by root tip". Any
	// fork qualifies while the root chain confirms no block of the shard yet,
	// as after genesis. pyquarkchain first checks that the block is higher than
	// C, which is implied here: the block is new, so it is not C, and it only
	// extends C if it is higher.
	confirmed, err := c.lastConfirmed(rootTip)
	if err != nil {
		return false, err
	}
	if confirmed != nil && !c.isShardDescendant(header, confirmed) {
		return false, nil
	}
	// Rule 4. On a full tie, the head stays.
	if header.Number.Uint64() != current.Number.Uint64() {
		return header.Number.Uint64() > current.Number.Uint64(), nil
	}

	// If both shard headers has the same block number,
	// compare the view of the root block and switch to the minor
	// block with longer root block for more cross shard tx fee.
	currentRoot, err := c.prevRootBlock(current)
	if err != nil {
		return false, fmt.Errorf("%w: current head: %w", shardInconsistencyErr, err)
	}
	return prevRoot.Number > currentRoot.Number, nil
}

// InsertRootBlock validates a root block against the shard chain, stores it,
// and makes it the root tip if the fork choice rule selects it, moving the
// canonical head onto the shard chain it confirms. It reports whether the root
// tip changed. Known root blocks reuse their stored confirmation and participate
// in fork choice, allowing an interrupted tip update to be retried. It mirrors
// ShardState.add_root_block in pyquarkchain.
//
// Unlike shard blocks, root blocks are chosen by total difficulty: "Switch to
// the root block with higher total diff". On a tie, the root tip stays.
func (c *ShardChain) InsertRootBlock(root *qkctypes.RootBlock) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rootTip := c.rootTip()
	if rootTip == nil {
		return false, ErrRootChainUninitialized
	}
	// Step 1: reuse the stored confirmation of a known root block, or validate
	// and persist a new root block together with its latest confirmed shard block.
	var confirmed *types.Header
	var err error
	if rawdb.HasRootBlock(c.db, root.Hash()) {
		confirmed, err = c.lastConfirmed(root.Header())
		if err != nil {
			return false, err
		}
	} else {
		confirmed, err = c.validateRootBlock(root)
		if err != nil {
			return false, err
		}
		batch := c.db.NewBatch()
		defer batch.Close()
		rawdb.WriteRootBlock(batch, root)
		if confirmed != nil {
			rawdb.WriteLastConfirmedBlockHash(batch, root.Hash(), confirmed.Hash())
		}
		if err := batch.Write(); err != nil {
			return false, fmt.Errorf("store root block %x: %w", root.Hash(), err)
		}
	}
	// Step 2: the fork choice.
	switched := root.TotalDifficulty().Cmp(&rootTip.TotalDifficulty) > 0
	// Step 3: reconcile the shard head before committing the root tip. On error,
	// the root tip stays unchanged, so the stored root block can retry the switch.
	if switched {
		if err := c.setHeadForRootTip(root.Header(), confirmed); err != nil {
			return false, err
		}
	}
	return switched, nil
}

// validateRootBlock checks a root block against the shard and root chains, and
// returns C, the latest shard block confirmed by the root chain up to it, or nil
// if none is confirmed yet. It mirrors the shard-specific checks of
// ShardState.add_root_block in pyquarkchain. The master must validate the root
// block and its body before forwarding it to shards. Each shard validates its
// minor blocks and distributes outgoing cross-shard deposits. This method checks
// availability and consistency of local shard data.
// Failures of pyquarkchain's check assertions wrap clusterInconsistencyErr;
// missing already-validated local data wraps shardInconsistencyErr. Ordinary
// input validation errors keep their specific error classification.
//
// Rule 1: the master must validate root consensus, including proof of work
// and total difficulty. Locally, the referenced parent must be available, and
// this block must be higher than the root block referenced by the shard genesis
// ("cannot find previous root block in pool"). The shard knows no root block
// below that one, so the genesis root is an ancestor of all later roots.
//
// Rule 2: the master must validate the commitment to the carried shard headers
// and their complete ordering/continuity. Locally, blocks for this shard must
// already be present ("cannot find minor block in local shard"), and their count
// must respect MaxBlocksPerRootBlock ("too many minor blocks in the root block").
// Missing local blocks are rejected so the caller can synchronize them and
// retry. The source shards must have delivered all required cross-shard deposits
// before this root block is imported. Missing or unexpected deposit lists violate
// that cluster guarantee ("cannot find x_shard tx list"), see
// validateXShardDeposits.
//
// Rule 3: locally, the first carried block of this shard must extend C', the
// latest one confirmed by the parent, and C, the last carried block (or C' if
// there are none), must reference an ancestor of the root block. Only the first
// carried block is checked against C' here; intermediate ordering/continuity is
// the master's responsibility ("Master should assure this check
// will not fail").
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
	// Master validation is the first guard for root consensus (including PoW
	// and total difficulty), body commitments and complete header continuity.
	// This shard checks local availability and consistency below; it does not
	// repeat those upstream checks or accept missing required local data.
	// The master is expected to reject unsupported versions too; check it
	// locally so malformed or misrouted root blocks cannot enter this shard DB.
	if header.Version != 0 {
		return nil, fmt.Errorf("%w: %d", ErrRootBlockVersion, header.Version)
	}
	// Rule 1: the root block is higher than the root block of the shard
	// genesis, which is known as the shard chain starts on it, and its parent
	// is known.
	genesisRoot, err := c.prevRootBlock(c.chain.Genesis().Header())
	if err != nil {
		return nil, fmt.Errorf("%w: genesis: %w", shardInconsistencyErr, err)
	}
	if header.Number <= genesisRoot.Number {
		return nil, fmt.Errorf("%w: %w: root #%d, genesis root #%d", clusterInconsistencyErr, ErrRootNotAfterGenesis, header.Number, genesisRoot.Number)
	}
	parent := rawdb.ReadRootBlockHeader(c.db, header.ParentHash)
	if parent == nil {
		return nil, fmt.Errorf("%w: parent %x", ErrUnknownRootBlock, header.ParentHash)
	}
	// Rule 2 local checks: require every carried local-shard header and relevant
	// cross-shard deposit list to be available before accepting this root block.
	// Do not skip unavailable data; the caller must sync it and retry. Preserve
	// the local headers' carried order for the first-block check in rule 3.
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
	// Rule 3 local continuity check. The master must validate the complete
	// carried-header ordering and continuity; here we check the first local block
	// against C', the parent's latest confirmation (or genesis if none).
	confirmed, err := c.lastConfirmed(parent)
	if err != nil {
		return nil, err
	}
	if len(blocks) > 0 {
		first := blocks[0]
		if confirmed == nil {
			genesis := c.chain.Genesis().Header()
			if first.Number.Uint64() != 0 || first.Hash() != genesis.Hash() {
				return nil, fmt.Errorf("%w: %w: first #%d [%x..]", clusterInconsistencyErr, ErrConfirmedNotContiguous, first.Number, first.Hash().Bytes()[:4])
			}
		} else if first.ParentHash != confirmed.Hash() {
			return nil, fmt.Errorf("%w: %w: first #%d [%x..]", clusterInconsistencyErr, ErrConfirmedNotContiguous, first.Number, first.Hash().Bytes()[:4])
		}
		confirmed = blocks[len(blocks)-1]
	}
	// Rule 3 local ancestry check: C, the last block of this shard, or C' if
	// there are none, references an ancestor of the root block. Even after
	// master validation, verify this against the locally stored root chain.
	if confirmed != nil {
		prevRoot, err := c.prevRootBlock(confirmed)
		if err != nil {
			return nil, fmt.Errorf("%w: confirmed block: %w", shardInconsistencyErr, err)
		}
		if !c.isRootDescendant(header, prevRoot) {
			return nil, fmt.Errorf("%w: %w: confirmed #%d [%x..] references root #%d [%x..]", clusterInconsistencyErr, ErrConfirmedRootNotAncestor,
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
	sends := prevRoot != nil && prevRoot.Number > genesisRoot.Number && config.IsXShardDestination(extra.ShardID)
	stored := rawdb.HasXShardDeposits(c.db, block.Hash())
	switch {
	case sends && !stored:
		return fmt.Errorf("%w: %w: shard %d block #%d [%x..]", clusterInconsistencyErr, ErrMissingXShardDeposits, extra.ShardID, block.Number, block.Hash().Bytes()[:4])
	case !sends && stored:
		return fmt.Errorf("%w: %w: shard %d block #%d [%x..]", clusterInconsistencyErr, ErrUnexpectedXShardDeposits, extra.ShardID, block.Number, block.Hash().Bytes()[:4])
	}
	return nil
}

// setHeadForRootTip moves the canonical head onto the shard chain of a stored
// root block and then commits it as the root tip, given C, the latest shard block
// confirmed by the root chain up to it, or nil if none. It mirrors the tip reset
// of ShardState.add_root_block in pyquarkchain. The head must keep the two
// properties shouldUpdateHead relies on: it extends C, and it references the
// root tip or one of its ancestors. Two cases apply.
//
// Case 1: C is not on the canonical chain, as the root tip confirms another
// fork. The head moves to C, which has both properties, see rule 3 of
// validateRootBlock. A descendant such as D can also be the shard tip if it
// references the new root tip or one of its ancestors; D need not itself be
// root-confirmed. As a simplification, switch to C instead of searching for
// the best compatible descendant, which would require additional fork indexing
// and selection logic ("shard_header might not be the tip of the longest chain"
// in pyquarkchain).
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
// Case 2: C is canonical, or no shard block has been confirmed yet (as with a
// root genesis that has no last-confirmed shard block). Starting at the current
// head, walk back to the nearest shard block whose referenced root is the new
// root tip or one of its ancestors. If C exists, it is the earliest block the
// walk can reach; if C does not exist, the walk can reach the shard genesis at
// most. This follows "we need to scan back until finding a minor block pointing
// to the same root chain r_block is on".
//
//	                     +----- R2'
//	                     v
//	root chain    R0 <-- R1 <-- R2 <-- R3
//	                     :
//	                     v
//	shard chain   G <--- C <--- X[R1] <--- Y[R2'] <--- H[R2']
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
	// Walk back to the nearest shard block referencing the new root tip or one
	// of its ancestors. If C exists, do not walk past it; otherwise, stop at the
	// shard genesis. Return ErrRootNotDescendant if the genesis creation root is
	// not on the new root chain.
	for {
		prevRoot, err := c.prevRootBlock(head)
		if err != nil {
			return fmt.Errorf("%w: shard head: %w", shardInconsistencyErr, err)
		}
		if c.isRootDescendant(rootTip, prevRoot) {
			// If C exists, root-block validation guarantees the scan matches by C
			// at the latest; Case 1 starts the scan at C itself.
			break
		}
		if head.Number.Uint64() == 0 {
			// With no C, genesis is the last candidate. Its creation root must
			// still be on the new root chain.
			return fmt.Errorf("%w: %w: genesis references root #%d [%x..]", shardInconsistencyErr, ErrRootNotDescendant, prevRoot.Number, prevRoot.Hash().Bytes()[:4])
		}
		if head = c.chain.GetHeader(head.ParentHash, head.Number.Uint64()-1); head == nil {
			return fmt.Errorf("%w: %w: shard head parent missing", shardInconsistencyErr, consensus.ErrUnknownAncestor)
		}
	}
	if head.Hash() != current.Hash() {
		block := c.chain.GetBlock(head.Hash(), head.Number.Uint64())
		if block == nil {
			return fmt.Errorf("%w: %w: head #%d [%x..]", shardInconsistencyErr, consensus.ErrUnknownAncestor, head.Number, head.Hash().Bytes()[:4])
		}
		if _, err := c.chain.SetCanonical(block); err != nil {
			return fmt.Errorf("set canonical head %x: %w", head.Hash(), err)
		}
	}
	// The root tip advances even when the shard head already matches. This
	// single-key write is fatal on failure, like the shard head's DB writes.
	// It is not atomic with SetCanonical: a crash between the writes can leave
	// the persisted shard head ahead of the root tip.
	// TODO: Before accepting block imports on startup, reconcile the shard head
	// against the committed root tip and its stored confirmation. Recovery must
	// be retryable if interrupted, and missing required data must prevent imports.
	rawdb.WriteRootHeadHash(c.db, rootTip.Hash())
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
		// InsertRootBlock only stores confirmations of locally available blocks.
		return nil, fmt.Errorf("%w: %w: %x", shardInconsistencyErr, ErrUnknownConfirmedBlock, hash)
	}
	return header, nil
}

// isRootDescendant reports whether the root block descendant extends, or is,
// the root block ancestor. It returns false if an ancestor is missing.
func (c *ShardChain) isRootDescendant(descendant, ancestor *qkctypes.RootBlockHeader) bool {
	for descendant.Number > ancestor.Number {
		if descendant = rawdb.ReadRootBlockHeader(c.db, descendant.ParentHash); descendant == nil {
			return false
		}
	}
	return descendant.Hash() == ancestor.Hash()
}

// isShardDescendant reports whether the shard block descendant extends, or is,
// the shard block ancestor. It returns false if an ancestor is missing.
func (c *ShardChain) isShardDescendant(descendant, ancestor *types.Header) bool {
	for descendant.Number.Uint64() > ancestor.Number.Uint64() {
		if descendant = c.chain.GetHeader(descendant.ParentHash, descendant.Number.Uint64()-1); descendant == nil {
			return false
		}
	}
	return descendant.Hash() == ancestor.Hash()
}
