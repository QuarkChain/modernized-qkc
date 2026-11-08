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
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

// EthashVerifier checks the PoW shared by root and shard consensus. It keeps two
// immutable epoch caches and has no background workers.
type EthashVerifier struct {
	cacheMu sync.Mutex
	caches  *lru.Cache[uint64, []uint32]
}

// NewEthashVerifier creates an empty verifier shared by root and shard engines.
func NewEthashVerifier() *EthashVerifier {
	return &EthashVerifier{caches: lru.NewCache[uint64, []uint32](2)}
}

// Close releases the cached verification data. No background workers are used.
func (v *EthashVerifier) Close() error {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	v.caches.Purge()
	return nil
}

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
	// The calculator requires a staking reader only when staking is enabled.
	reader, _ := chain.(consensus.StakingReader)
	difficulty, err := GetAdjustedDifficulty(p.config.QuarkChain.PoSW, reader, header)
	if err != nil {
		return err
	}
	return p.EthashVerifier.Verify(header.Number.Uint64(), hash, extra.Nonce.Uint64(), extra.MixHash, difficulty)
}

// Verify checks positive effective difficulty, the Ethash mix digest and whether
// the PoW result meets the target for an already validated header.
// number is the new-chain height; the regenesis offset applies only to decay.
func (v *EthashVerifier) Verify(number uint64, hash common.Hash, nonce uint64, mixHash common.Hash, difficulty *big.Int) error {
	if difficulty == nil || difficulty.Sign() <= 0 {
		return errInvalidDifficulty
	}
	// Recompute the mix and result from the seal hash and nonce at this height.
	digest, result := hashimotoLight(datasetSize(number), v.cache(number), hash[:], nonce)
	if !bytes.Equal(digest, mixHash[:]) {
		return errors.New("invalid Ethash mix hash")
	}
	// Match pyquarkchain: result <= floor(2^256 / effective difficulty).
	target := new(big.Int).Div(new(big.Int).Lsh(big.NewInt(1), 256), difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errors.New("invalid Ethash proof of work")
	}
	return nil
}

// cache retains two immutable epoch caches. Serialize generation so concurrent
// imports cannot allocate duplicate caches; hashimoto runs outside this lock.
func (v *EthashVerifier) cache(number uint64) []uint32 {
	v.cacheMu.Lock()
	defer v.cacheMu.Unlock()
	epoch := number / epochLength
	if cache, ok := v.caches.Get(epoch); ok {
		return cache
	}
	cache := make([]uint32, cacheSize(number)/4)
	generateCache(cache, epoch, seedHash(number))
	v.caches.Add(epoch, cache)
	return cache
}
