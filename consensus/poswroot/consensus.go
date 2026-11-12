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

// Package poswroot implements root-chain Ethash consensus with a staking discount.
package poswroot

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// Match pyquarkchain's ALLOWED_FUTURE_BLOCKS_TIME_VALIDATION.
const futureTime = 15

// PoSW validates roots without changing the block or the reader's stored data.
// The configuration is checked once by New and must remain unchanged afterward.
type PoSW struct {
	*posw.EthashVerifier
	config   *params.QuarkChainConfig
	members  map[string]bool
	simulate bool // Set only by NewSimulate; skips Ethash verification.
}

var _ RootEngine = (*PoSW)(nil)

// New checks the root consensus settings and the shared shard network settings.
func New(config *params.QuarkChainConfig) (*PoSW, error) {
	if err := config.Check(); err != nil {
		return nil, err
	}
	members := make(map[string]bool, len(config.ChainIDs))
	for _, id := range config.ChainIDs {
		members[id.String()] = true
	}
	return &PoSW{EthashVerifier: posw.NewEthashVerifier(), config: config, members: members}, nil
}

// NewSimulate keeps root consensus rules while skipping Ethash verification.
// TODO: Select this engine through chain consensus configuration in RootChain's
// New/Init flow; default to PoSW and require all nodes to use the same setting.
func NewSimulate(config *params.QuarkChainConfig) (*PoSW, error) {
	engine, err := New(config)
	if err != nil {
		return nil, err
	}
	engine.simulate = true
	return engine, nil
}

func (p *PoSW) Author(header *qkctypes.RootBlockHeader) (common.Address, error) {
	if header == nil {
		return common.Address{}, errors.New("missing root header")
	}
	return header.Coinbase, nil
}

// VerifyBlock validates a non-genesis root. Genesis and its shard confirmation
// records are established by the root-chain initialization code.
func (p *PoSW) VerifyBlock(chain RootChainReader, block *qkctypes.RootBlock) error {
	if chain == nil {
		return errors.New("missing root chain reader")
	}
	if block == nil {
		return errors.New("missing root block")
	}
	// Use the candidate's actual parent for all checks, including on side branches.
	header := block.Header()
	parent, err := chain.GetRootHeader(header.ParentHash)
	if err != nil {
		return err
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	err = p.verifyHeader(header, parent)
	if err != nil {
		return err
	}
	// Verify the root's PoW seal before body validation, as in pyquarkchain.
	// Checking shard headers' PrevRootBlock references may require walking many
	// root ancestors; blocks with invalid PoW should not trigger those reads.
	if err := p.verifySeal(chain, header); err != nil {
		return err
	}
	return p.verifyBody(chain, block, parent)
}

// verifyHeader checks local field limits, parent linkage, height, timestamps,
// base difficulty and total difficulty. Body contents and PoW are checked later.
func (p *PoSW) verifyHeader(header, parent *qkctypes.RootBlockHeader) error {
	// Check version, positive difficulty, the total-difficulty floor and Extra size.
	if err := header.SanityCheck(); err != nil {
		return err
	}
	// Normal imports exclude genesis and must extend the requested parent by one.
	if header.Number == 0 {
		return consensus.ErrInvalidNumber
	}
	if parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if header.Number != parent.Number+1 {
		return consensus.ErrInvalidNumber
	}
	// Time must advance from the parent and stay within the allowed clock skew.
	if header.Time <= parent.Time {
		return errors.New("root timestamp must be greater than parent")
	}
	now := uint64(time.Now().Unix())
	if header.Time > now && header.Time-now > futureTime {
		return consensus.ErrFutureBlock
	}
	// The header records base difficulty; staking discounts apply only to the seal.
	difficulty, overflow := uint256.FromBig(p.CalcDifficulty(header.Time, parent))
	if overflow || difficulty.IsZero() || difficulty.Cmp(&header.Difficulty) != 0 {
		return errors.New("invalid root base difficulty")
	}
	// Fork-choice weight adds base difficulty without wrapping uint256.
	total, overflow := new(uint256.Int).AddOverflow(&parent.TotalDifficulty, difficulty)
	if overflow || total.Cmp(&header.TotalDifficulty) != 0 {
		return errors.New("invalid or overflowing root total difficulty")
	}
	return nil
}

// verifyBody checks the shard commitment, known blocks and reward, then grouping,
// confirmation links, timestamps, per-shard limits and root references.
// Shards are responsible for validating their blocks' execution and PoW.
func (p *PoSW) verifyBody(chain RootChainReader, block *qkctypes.RootBlock, parent *qkctypes.RootBlockHeader) error {
	// Match pyquarkchain root_state.validate_block's tracking-data size limit.
	if len(block.TrackingData()) > 1024 {
		return errors.New("root tracking data exceeds 1024 bytes")
	}
	// Anchor every shard to this parent branch's cumulative confirmation records,
	// including confirmations inherited from roots before the parent.
	lastConfirmed, err := chain.GetLastConfirmedShardBlocks(parent.Hash())
	if err != nil {
		return err
	}
	headers := block.ShardBlockHeaders()
	// CalcShardHeaderHash panics on nil headers, so reject them before hashing.
	for _, header := range headers {
		if header == nil {
			return errors.New("missing shard header")
		}
	}
	// Bind the ordered shard header list to the commitment in the root header.
	if qkctypes.CalcShardHeaderHash(headers) != block.ShardHeaderHash() {
		return errors.New("invalid root shard header hash")
	}
	// Computing the reward also requires a reported coinbase amount for every shard
	// block, confirming that each has passed shard validation. Reject unknown shard
	// blocks here before the checks below use their fields or follow PrevRootBlock
	// references through root ancestors.
	amount, err := p.coinbaseAmount(chain, block)
	if err != nil {
		return err
	}
	if amount.Cmp(block.CoinbaseAmount()) != 0 {
		return errors.New("invalid root coinbase amount")
	}
	// Cache only root references verified against this parent, not intermediate
	// ancestors. The parent itself is already known to be on this branch.
	verifiedRootHashes := map[common.Hash]bool{parent.Hash(): true}
	var previous *types.Header
	var previousHash common.Hash
	var chainID *big.Int
	var count uint64
	for i, header := range headers {
		hash := header.Hash()
		extra, err := header.QKCExtra()
		if err != nil {
			return fmt.Errorf("shard header %d: %w", i, err)
		}
		// A root cannot confirm a shard block created after that root.
		if header.Time > block.Time() {
			return errors.New("shard timestamp is later than root timestamp")
		}
		// Group shard headers by increasing chain ID, with consecutive block numbers
		// within each group. Reject chain IDs not configured for this network.
		if chainID != nil && extra.ChainID.Cmp(chainID) < 0 {
			return errors.New("shard headers must be grouped by increasing chain ID")
		}
		if chainID == nil || extra.ChainID.Cmp(chainID) != 0 {
			chainID, count = extra.ChainID, 0
			if !p.members[chainID.String()] {
				return fmt.Errorf("unknown shard chain ID: %s", chainID)
			}
			// Each group starts from this parent root block's confirmation. All shards
			// start at root genesis; a missing baseline cannot initialize one here.
			hash, ok := lastConfirmed[chainID.String()]
			if !ok || hash == (common.Hash{}) {
				return errors.New("missing parent root shard confirmation")
			}
			previousHash, previous = hash, nil
		}
		// The first header's ParentHash must match the shard block last confirmed
		// by the parent root. Shard validation has already checked its block number.
		// Each later header must point to the preceding header in this group and
		// have a block number exactly one higher.
		if header.Number == nil || header.ParentHash != previousHash ||
			(previous != nil && header.Number.Cmp(new(big.Int).Add(previous.Number, common.Big1)) != 0) {
			return errors.New("shard headers do not continue the previous header by height and hash")
		}
		count++
		// A root block must include at most MaxBlocksPerRootBlock shard headers
		// for each chain ID.
		if count > p.config.MaxBlocksPerRootBlock {
			return errors.New("too many shard headers for one chain")
		}
		// The shard block's previous root must be the parent root or one of its ancestors,
		// never a root on another branch or above the candidate's parent.
		if !verifiedRootHashes[extra.PrevRootBlock] {
			sameChain, err := isRootDescendant(chain, parent, extra.PrevRootBlock)
			if err != nil {
				return err
			}
			if !sameChain {
				return errors.New("shard previous root is not an ancestor of the parent root")
			}
			verifiedRootHashes[extra.PrevRootBlock] = true
		}
		previousHash, previous = hash, header
	}
	return nil
}

// isRootDescendant checks whether descendant extends, or is, ancestorHash.
// It walks parent links like pyquarkchain's is_same_chain. Block/header
// caching belongs to RootChain.
func isRootDescendant(chain RootChainReader, descendant *qkctypes.RootBlockHeader, ancestorHash common.Hash) (bool, error) {
	// Missing data is a read error, rather than proof that the roots are unrelated.
	ancestor, err := chain.GetRootHeader(ancestorHash)
	if err != nil {
		return false, err
	}
	if ancestor == nil {
		return false, consensus.ErrUnknownAncestor
	}
	if ancestor.Hash() != ancestorHash {
		return false, errors.New("root reader returned a different ancestor")
	}
	// Follow this descendant's branch until the claimed ancestor's height.
	for descendant.Number > ancestor.Number {
		previous, err := chain.GetRootHeader(descendant.ParentHash)
		if err != nil {
			return false, err
		}
		if previous == nil {
			return false, consensus.ErrUnknownAncestor
		}
		// Each read must return the exact parent at the preceding height.
		if previous.Number+1 != descendant.Number || previous.Hash() != descendant.ParentHash {
			return false, errors.New("inconsistent root ancestry")
		}
		descendant = previous
	}
	// Equal heights alone do not establish ancestry: a side branch has another hash.
	return descendant.Hash() == ancestorHash, nil
}

// Prepare fills base difficulty and total difficulty only. The staking discount
// affects the seal target and never changes these fork-choice fields.
func (p *PoSW) Prepare(chain RootChainReader, header *qkctypes.RootBlockHeader) error {
	if chain == nil || header == nil {
		return errors.New("missing root reader or header")
	}
	parent, err := chain.GetRootHeader(header.ParentHash)
	if err != nil {
		return err
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if header.Number != parent.Number+1 {
		return consensus.ErrInvalidNumber
	}
	if header.Time <= parent.Time {
		return errors.New("root timestamp must be greater than parent")
	}
	difficulty, overflow := uint256.FromBig(p.CalcDifficulty(header.Time, parent))
	if overflow || difficulty.IsZero() {
		return errors.New("root difficulty exceeds uint256")
	}
	total, overflow := new(uint256.Int).AddOverflow(&parent.TotalDifficulty, difficulty)
	if overflow {
		return errors.New("root total difficulty exceeds uint256")
	}
	header.Difficulty, header.TotalDifficulty = *difficulty, *total
	return nil
}

// Finalize prepares a root block for mining. It calculates the reward, then calls
// RootBlock.Finalize to update the amount and shard-header commitment in place,
// keeping the block's coinbase address. Errors leave the block unchanged.
func (p *PoSW) Finalize(chain RootChainReader, block *qkctypes.RootBlock) (*qkctypes.RootBlock, error) {
	if block == nil {
		return nil, errors.New("missing root block")
	}
	if chain == nil {
		return nil, errors.New("missing root chain reader")
	}
	header := block.Header()
	if err := header.SanityCheck(); err != nil {
		return nil, err
	}
	amount, err := p.coinbaseAmount(chain, block)
	if err != nil {
		return nil, err
	}
	return block.Finalize(amount, &header.Coinbase), nil
}

func (p *PoSW) SealHash(header *qkctypes.RootBlockHeader) common.Hash {
	return header.SealHash()
}

// CalcDifficulty uses the shared adjustment with this root chain's parameters.
// Callers must first ensure timestamp is strictly later than the parent's time.
func (p *PoSW) CalcDifficulty(timestamp uint64, parent *qkctypes.RootBlockHeader) *big.Int {
	return posw.CalcDifficulty(p.config.PoSW, timestamp, parent.Time, parent.Difficulty.ToBig())
}

func (p *PoSW) Close() error { return p.EthashVerifier.Close() }

// coinbaseAmount requires reported shard amounts, sums them, then applies t/(1-t)
// and rounds once. A non-nil amount identifies a validated shard block.
// The root's own decayed subsidy is added without reward tax.
// Finalize and verifyBody share this read-only calculation so their rewards agree.
func (p *PoSW) coinbaseAmount(chain RootChainReader, block *qkctypes.RootBlock) (*uint256.Int, error) {
	shardAmount := new(big.Int)
	for i, header := range block.ShardBlockHeaders() {
		if header == nil {
			return nil, errors.New("missing shard header")
		}
		hash := header.Hash()
		amount, err := chain.GetShardBlockCoinbaseAmount(hash)
		if err != nil {
			return nil, fmt.Errorf("shard header %d: %w", i, err)
		}
		if amount == nil {
			return nil, fmt.Errorf("unknown shard block: %s", hash)
		}
		shardAmount.Add(shardAmount, amount.ToBig())
	}
	tax := p.config.RewardTaxRate
	amount := new(big.Int).Mul(shardAmount, tax.Num())
	amount.Div(amount, new(big.Int).Sub(tax.Denom(), tax.Num()))
	amount.Add(amount, p.config.PoSW.BlockRewardAt(new(big.Int).SetUint64(uint64(block.Number()))))
	result, overflow := uint256.FromBig(amount)
	if overflow {
		return nil, fmt.Errorf("root coinbase amount exceeds uint256")
	}
	return result, nil
}
