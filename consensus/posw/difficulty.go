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

package posw

import (
	"math/big"

	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

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

// GetAdjustedDifficulty applies the same stake quota rule on both chains. Mined
// counts all blocks by this coinbase among the preceding windowSize-1 ancestors,
// including ordinary PoW blocks. The candidate occupies the last window slot.
// Stakes and the window must come from the candidate's parent fork/state.
// number is the candidate's height on the new chain; the config applies the old height offset.
// The configuration determines whether the stake requirement decays by epoch.
func GetAdjustedDifficulty(config *params.PoSWConfig, number, difficulty *big.Int, stakes *uint256.Int, mined uint64) *big.Int {
	diff := new(big.Int).Set(difficulty)
	if config == nil || !config.StakingEnabled || stakes == nil || number.Sign() == 0 {
		return diff
	}
	quota := new(big.Int).Div(stakes.ToBig(), config.StakePerBlockAt(number))
	// Compare before narrowing: large stakes must not wrap a uint64 quota.
	threshold := config.WindowSize
	if quota.Cmp(new(big.Int).SetUint64(threshold)) < 0 {
		threshold = quota.Uint64()
	}
	if mined < threshold {
		diff.Div(diff, new(big.Int).SetUint64(config.DiffDivider))
		if diff.Sign() == 0 {
			diff.SetInt64(1)
		}
	}
	return diff
}
