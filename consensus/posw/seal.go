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

package posw

import (
	"bytes"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

// VerifySeal checks Ethash against the parent-state PoSW target. Header-only
// verification deliberately does not call this; full body import must call it.
// The header must already pass header validation, and its parent state must be available.
func (p *PoSW) VerifySeal(chain consensus.ChainHeaderReader, header *types.Header) error {
	extra, err := header.QKCExtra()
	if err != nil {
		return err
	}
	hash, err := header.QKCSealHash()
	if err != nil {
		return err
	}
	difficulty, err := p.adjustedDifficulty(chain, header)
	if err != nil {
		return err
	}
	if difficulty.Sign() <= 0 {
		return errInvalidDifficulty
	}
	// Ethash epochs use the new-chain height. OriginalBlockNumber applies only
	// to reward and staking decay, not to Ethash cache or dataset selection.
	number := header.Number.Uint64()
	// Recompute the actual seal stored in QKCExtra, not geth's Nonce/MixDigest.
	digest, result := hashimotoLight(datasetSize(number), p.cache(number), hash[:], extra.Nonce.Uint64())
	if !bytes.Equal(digest, extra.MixHash[:]) {
		return errors.New("invalid Ethash mix hash")
	}
	// Match pyquarkchain: result <= floor(2^256 / effective difficulty).
	target := new(big.Int).Div(new(big.Int).Lsh(big.NewInt(1), 256), difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errors.New("invalid Ethash proof of work")
	}
	return nil
}

// adjustedDifficulty combines the miner's parent-state stake with its recent
// block count on the candidate's fork, without changing the header's base difficulty.
func (p *PoSW) adjustedDifficulty(chain consensus.ChainHeaderReader, header *types.Header) (*big.Int, error) {
	difficulty := header.QKCDifficulty()
	if difficulty.Sign() <= 0 {
		return nil, errInvalidDifficulty
	}
	config := p.config.PoSW
	if !config.StakingEnabled {
		// Ordinary PoW needs neither a staking reader nor a window traversal.
		return difficulty, nil
	}
	reader, ok := chain.(consensus.StakingReader)
	if !ok {
		return nil, errors.New("shard staking reader unavailable")
	}
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return nil, consensus.ErrUnknownAncestor
	}
	// Pin the read to the actual parent, including on side forks. Candidate
	// transactions, deposits and rewards must not affect its staking eligibility.
	stakes, err := reader.GetShardLockedStakes(parent, header.Coinbase)
	if err != nil {
		return nil, err
	}
	// An address with no locked stake returns a non-nil zero value.
	// Nil indicates an invalid reader result, not an unstaked miner.
	if stakes == nil {
		return nil, errors.New("missing shard staking result")
	}
	// Count all blocks by this coinbase on the candidate's fork, including genesis
	// when it lies inside the window, just as pyquarkchain's block counter does.
	var mined uint64
	for i, ancestor := uint64(0), parent; i < config.WindowSize-1; i++ {
		if ancestor.Coinbase == header.Coinbase {
			mined++
		}
		// Do not require an ancestor outside the configured history window.
		if ancestor.Number.Sign() == 0 || i+1 == config.WindowSize-1 {
			break
		}
		ancestor = chain.GetHeader(ancestor.ParentHash, ancestor.Number.Uint64()-1)
		if ancestor == nil {
			return nil, consensus.ErrUnknownAncestor
		}
	}
	return GetAdjustedDifficulty(config, header.Number, difficulty, stakes, mined), nil
}

// cache retains two immutable epoch caches. Serialize generation so concurrent
// imports cannot allocate duplicate caches; hashimoto runs outside this lock.
func (p *PoSW) cache(number uint64) []uint32 {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	epoch := number / epochLength
	if cache, ok := p.caches.Get(epoch); ok {
		return cache
	}
	cache := make([]uint32, cacheSize(number)/4)
	generateCache(cache, epoch, seedHash(number))
	p.caches.Add(epoch, cache)
	return cache
}
