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

package poswshard

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

var errInvalidDifficulty = errors.New("invalid shard PoW difficulty")

// CalcDifficulty is shared by root and shard consensus. It is Homestead's
// adjustment with configurable parameters and no difficulty bomb, matching
// goquarkchain's EthDifficultyCalculator. Before calling, header validation must
// reject timestamp <= parentTimestamp, and block producers must choose a strictly
// later timestamp. Like consensus.Engine.CalcDifficulty, this function calculates
// difficulty for valid inputs and does not return header validation errors.
func CalcDifficulty(config *params.PoSWConfig, timestamp, parentTimestamp uint64, parentDifficulty *big.Int) *big.Int {
	adjustment := new(big.Int).SetUint64((timestamp - parentTimestamp) / config.AdjustmentCutoff)
	adjustment.Sub(big.NewInt(1), adjustment)
	if adjustment.Cmp(big.NewInt(-99)) < 0 {
		adjustment.SetInt64(-99)
	}
	diff := new(big.Int).Div(parentDifficulty, new(big.Int).SetUint64(config.AdjustmentFactor))
	diff.Mul(diff, adjustment).Add(diff, parentDifficulty)
	if diff.Cmp(config.MinimumDifficulty) < 0 {
		diff.Set(config.MinimumDifficulty)
	}
	return diff
}

// GetAdjustedDifficulty returns a shard candidate's effective seal difficulty
// using its actual parent state and fork history, without modifying the header.
// config must pass Check, and header must pass header validation before use.
// With staking disabled, reader is unused and may be nil.
func GetAdjustedDifficulty(config *params.PoSWConfig, reader consensus.StakingReader, header *types.Header) (*big.Int, error) {
	difficulty := header.QKCDifficulty()
	if difficulty.Sign() <= 0 {
		return nil, errInvalidDifficulty
	}
	if !config.StakingEnabled {
		// Ordinary PoW needs neither a staking reader nor a window traversal.
		return difficulty, nil
	}
	if reader == nil {
		return nil, errors.New("shard staking reader unavailable")
	}
	parent := reader.GetHeader(header.ParentHash, header.Number.Uint64()-1)
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
		ancestor = reader.GetHeader(ancestor.ParentHash, ancestor.Number.Uint64()-1)
		if ancestor == nil {
			return nil, consensus.ErrUnknownAncestor
		}
	}
	return CalcAdjustedDifficulty(config, header.Number, difficulty, stakes, mined), nil
}

// StakeQuota returns the number of discounted blocks allowed by locked stakes,
// capped at WindowSize. Root and shard consensus share this quota calculation.
// config must pass Check with staking enabled, and number and stakes must be non-nil.
func StakeQuota(config *params.PoSWConfig, number *big.Int, stakes *uint256.Int) uint64 {
	quota := new(big.Int).Div(stakes.ToBig(), config.StakePerBlockAt(number))
	// Compare before narrowing: large stakes must not wrap a uint64 quota.
	if quota.Cmp(new(big.Int).SetUint64(config.WindowSize)) < 0 {
		return quota.Uint64()
	}
	return config.WindowSize
}

// DiscountDifficulty divides difficulty by diffDivider, with a minimum of 1,
// without modifying difficulty. Callers determine whether the candidate qualifies.
// diffDivider and difficulty must be positive.
func DiscountDifficulty(diffDivider uint64, difficulty *big.Int) *big.Int {
	diff := new(big.Int).Div(difficulty, new(big.Int).SetUint64(diffDivider))
	if diff.Sign() == 0 {
		// A large divider must not turn PoW difficulty into zero.
		diff.SetInt64(1)
	}
	return diff
}

// CalcAdjustedDifficulty applies the stake quota rule to a shard candidate.
//
// config supplies the staking rules. StakePerBlockAt provides the effective stake
// requirement and handles the configured height offset and decay rules.
//
// number is the candidate's block height after regenesis.
//
// difficulty is the candidate's base PoW difficulty, before the staking discount.
//
// stakes come from the parent shard block's state.
//
// mined counts all blocks by this coinbase among the preceding windowSize-1
// ancestors, including ordinary PoW blocks. Count ancestors on the candidate's
// own chain, starting at its parent. The candidate occupies the last window slot.
func CalcAdjustedDifficulty(config *params.PoSWConfig, number, difficulty *big.Int, stakes *uint256.Int, mined uint64) *big.Int {
	if config == nil || !config.StakingEnabled || stakes == nil || number.Sign() == 0 {
		return new(big.Int).Set(difficulty)
	}
	// Grant a discount only while the preceding blocks have not used all quota.
	if mined < StakeQuota(config, number, stakes) {
		return DiscountDifficulty(config.DiffDivider, difficulty)
	}
	return new(big.Int).Set(difficulty)
}
