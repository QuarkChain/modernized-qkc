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

package params

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common/math"
)

// MarshalJSON emits integers and ratios as unquoted decimal numbers.
func (c PoSWConfig) MarshalJSON() ([]byte, error) {
	type configJSON struct {
		AdjustmentCutoff        uint64       `json:"adjustmentCutoff"`
		AdjustmentFactor        uint64       `json:"adjustmentFactor"`
		MinimumDifficulty       *big.Int     `json:"minimumDifficulty"`
		CoinbaseAmount          *big.Int     `json:"coinbaseAmount"`
		RewardTaxRate           *json.Number `json:"rewardTaxRate"`
		EpochInterval           uint64       `json:"epochInterval"`
		DecayFactor             *json.Number `json:"decayFactor"`
		OriginalBlockNumber     uint64       `json:"originalBlockNumber,omitempty"`
		StakingEnabled          bool         `json:"stakingEnabled"`
		WindowSize              uint64       `json:"windowSize"`
		DiffDivider             uint64       `json:"diffDivider"`
		StakePerBlock           *big.Int     `json:"stakePerBlock"`
		PoSWStakingDecayEnabled bool         `json:"poswStakingDecayEnabled"`
	}
	tax, err := ratioToJSON(c.RewardTaxRate)
	if err != nil {
		return nil, fmt.Errorf("posw: rewardTaxRate: %w", err)
	}
	decay, err := ratioToJSON(c.DecayFactor)
	if err != nil {
		return nil, fmt.Errorf("posw: decayFactor: %w", err)
	}
	enc := configJSON{
		AdjustmentCutoff:        c.AdjustmentCutoff,
		AdjustmentFactor:        c.AdjustmentFactor,
		MinimumDifficulty:       c.MinimumDifficulty,
		CoinbaseAmount:          c.CoinbaseAmount,
		RewardTaxRate:           tax,
		EpochInterval:           c.EpochInterval,
		DecayFactor:             decay,
		OriginalBlockNumber:     c.OriginalBlockNumber,
		StakingEnabled:          c.StakingEnabled,
		WindowSize:              c.WindowSize,
		DiffDivider:             c.DiffDivider,
		StakePerBlock:           c.StakePerBlock,
		PoSWStakingDecayEnabled: c.PoSWStakingDecayEnabled,
	}
	return json.Marshal(&enc)
}

// UnmarshalJSON accepts decimal numbers, decimal strings and hex strings for
// integers, as core.Genesis does. Call Check separately to validate the rules.
func (c *PoSWConfig) UnmarshalJSON(input []byte) error {
	type configJSON struct {
		AdjustmentCutoff        *math.HexOrDecimal64  `json:"adjustmentCutoff"`
		AdjustmentFactor        *math.HexOrDecimal64  `json:"adjustmentFactor"`
		MinimumDifficulty       *math.HexOrDecimal256 `json:"minimumDifficulty"`
		CoinbaseAmount          *math.HexOrDecimal256 `json:"coinbaseAmount"`
		RewardTaxRate           *json.Number          `json:"rewardTaxRate"`
		EpochInterval           *math.HexOrDecimal64  `json:"epochInterval"`
		DecayFactor             *json.Number          `json:"decayFactor"`
		OriginalBlockNumber     *math.HexOrDecimal64  `json:"originalBlockNumber,omitempty"`
		StakingEnabled          *bool                 `json:"stakingEnabled"`
		WindowSize              *math.HexOrDecimal64  `json:"windowSize"`
		DiffDivider             *math.HexOrDecimal64  `json:"diffDivider"`
		StakePerBlock           *math.HexOrDecimal256 `json:"stakePerBlock"`
		PoSWStakingDecayEnabled *bool                 `json:"poswStakingDecayEnabled"`
	}
	var dec configJSON
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	tax, err := ratioFromJSON(dec.RewardTaxRate)
	if err != nil {
		return fmt.Errorf("posw: rewardTaxRate: %w", err)
	}
	decay, err := ratioFromJSON(dec.DecayFactor)
	if err != nil {
		return fmt.Errorf("posw: decayFactor: %w", err)
	}
	if dec.AdjustmentCutoff != nil {
		c.AdjustmentCutoff = uint64(*dec.AdjustmentCutoff)
	}
	if dec.AdjustmentFactor != nil {
		c.AdjustmentFactor = uint64(*dec.AdjustmentFactor)
	}
	if dec.MinimumDifficulty != nil {
		c.MinimumDifficulty = (*big.Int)(dec.MinimumDifficulty)
	}
	if dec.CoinbaseAmount != nil {
		c.CoinbaseAmount = (*big.Int)(dec.CoinbaseAmount)
	}
	if dec.RewardTaxRate != nil {
		c.RewardTaxRate = tax
	}
	if dec.EpochInterval != nil {
		c.EpochInterval = uint64(*dec.EpochInterval)
	}
	if dec.DecayFactor != nil {
		c.DecayFactor = decay
	}
	if dec.OriginalBlockNumber != nil {
		c.OriginalBlockNumber = uint64(*dec.OriginalBlockNumber)
	}
	if dec.StakingEnabled != nil {
		c.StakingEnabled = *dec.StakingEnabled
	}
	if dec.WindowSize != nil {
		c.WindowSize = uint64(*dec.WindowSize)
	}
	if dec.DiffDivider != nil {
		c.DiffDivider = uint64(*dec.DiffDivider)
	}
	if dec.StakePerBlock != nil {
		c.StakePerBlock = (*big.Int)(dec.StakePerBlock)
	}
	if dec.PoSWStakingDecayEnabled != nil {
		c.PoSWStakingDecayEnabled = *dec.PoSWStakingDecayEnabled
	}
	return nil
}

func ratioFromJSON(value *json.Number) (*big.Rat, error) {
	if value == nil {
		return nil, nil
	}
	ratio, ok := new(big.Rat).SetString(value.String())
	if !ok {
		return nil, fmt.Errorf("invalid ratio %q", value.String())
	}
	return ratio, nil
}

// ratioToJSON emits the exact decimal value, without rounding or fraction strings.
func ratioToJSON(value *big.Rat) (*json.Number, error) {
	if value == nil {
		return nil, nil
	}
	precision, exact := value.FloatPrec()
	if !exact {
		return nil, fmt.Errorf("ratio %s has no finite decimal representation", value)
	}
	number := json.Number(value.FloatString(precision))
	return &number, nil
}
