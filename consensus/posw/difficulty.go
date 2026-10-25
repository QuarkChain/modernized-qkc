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
// goquarkchain's EthDifficultyCalculator. Callers validate timestamps first.
func CalcDifficulty(config *params.PoWConfig, timestamp, parentTimestamp uint64, parentDifficulty *big.Int) *big.Int {
	adjustment := new(big.Int).SetUint64((timestamp - parentTimestamp) / config.AdjustmentCutoff)
	adjustment.Sub(big.NewInt(1), adjustment)
	if adjustment.Cmp(big.NewInt(-99)) < 0 {
		adjustment.SetInt64(-99)
	}
	diff := new(big.Int).Div(parentDifficulty, new(big.Int).SetUint64(config.AdjustmentFactor))
	diff.Mul(diff, adjustment).Add(diff, parentDifficulty)
	if minimum := config.MinimumDifficulty.ToBig(); diff.Cmp(minimum) < 0 {
		return minimum
	}
	return diff
}

// GetAdjustedDifficulty applies the same stake quota rule on both chains. Mined
// counts all blocks by this coinbase among the preceding windowSize-1 ancestors,
// including ordinary PoW blocks. The candidate occupies the last window slot.
// Stakes and the window must come from the candidate's parent fork/state.
func GetAdjustedDifficulty(config *params.PoSWConfig, difficulty *big.Int, stakes *uint256.Int, mined uint64) *big.Int {
	diff := new(big.Int).Set(difficulty)
	if config == nil || !config.Enabled || stakes == nil {
		return diff
	}
	quota := new(uint256.Int).Div(stakes, config.StakePerBlock)
	// Compare before narrowing: large stakes must not wrap a uint64 quota.
	threshold := config.WindowSize
	if quota.LtUint64(threshold) {
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
