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
)

// MarshalJSON emits the reward tax rate as an exact, unquoted decimal number.
func (c QuarkChainConfig) MarshalJSON() ([]byte, error) {
	type configJSON QuarkChainConfig
	tax, err := ratioToJSON(c.RewardTaxRate)
	if err != nil {
		return nil, fmt.Errorf("quarkchain: rewardTaxRate: %w", err)
	}
	return json.Marshal(&struct {
		*configJSON
		RewardTaxRate *json.Number `json:"rewardTaxRate"`
	}{(*configJSON)(&c), tax})
}

// UnmarshalJSON accepts decimal numbers and strings, including the rational
// strings persisted by older versions, without float64 rounding. Omitted or null
// rates leave the existing value unchanged. Call Check separately to validate
// the consensus rules.
func (c *QuarkChainConfig) UnmarshalJSON(input []byte) error {
	type configJSON QuarkChainConfig
	config := configJSON(*c)
	dec := struct {
		*configJSON
		RewardTaxRate *json.RawMessage `json:"rewardTaxRate"`
	}{configJSON: &config}
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	if dec.RewardTaxRate != nil {
		value := string(*dec.RewardTaxRate)
		if value[0] == '"' {
			if err := json.Unmarshal(*dec.RewardTaxRate, &value); err != nil {
				return fmt.Errorf("quarkchain: rewardTaxRate: %w", err)
			}
		}
		tax, ok := new(big.Rat).SetString(value)
		if !ok {
			return fmt.Errorf("quarkchain: rewardTaxRate: invalid ratio %q", value)
		}
		config.RewardTaxRate = tax
	}
	*c = QuarkChainConfig(config)
	return nil
}
