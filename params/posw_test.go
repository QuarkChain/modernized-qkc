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
	"math/big"
	"reflect"
	"testing"
)

func validPoSWConfig() *PoSWConfig {
	return &PoSWConfig{
		AdjustmentCutoff: 7, AdjustmentFactor: 512, MinimumDifficulty: big.NewInt(1),
		CoinbaseAmount: big.NewInt(100), EpochInterval: 10, DecayFactor: "0.88", RewardTaxRate: "0.5",
		StakingEnabled: true, WindowSize: 256, StakePerBlock: big.NewInt(100), DiffDivider: 20,
	}
}

func TestNewTestQuarkChainConfigIsolation(t *testing.T) {
	shardIDs := []uint32{0, 1}
	first := NewTestQuarkChainConfig(1, shardIDs...)
	second := NewTestQuarkChainConfig(1, shardIDs...)
	if err := second.PoSW.Check(); err != nil {
		t.Fatal(err)
	}
	first.PoSW.MinimumDifficulty.SetInt64(9)
	first.PoSW.CoinbaseAmount.SetInt64(9)
	first.ShardIDs[0] = 9
	shardIDs[1] = 9
	if second.PoSW.MinimumDifficulty.Int64() != 1 || second.PoSW.CoinbaseAmount.Sign() != 0 ||
		!reflect.DeepEqual(second.ShardIDs, []uint32{0, 1}) || first.ShardIDs[1] != 1 {
		t.Fatal("test configurations share mutable defaults or the caller's shard IDs")
	}
}

func TestPoSWConfigJSON(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		valid bool
	}{
		{
			"numbers",
			`{"posw": {
				"adjustmentCutoff": 7,
				"adjustmentFactor": 512,
				"minimumDifficulty": 100,
				"coinbaseAmount": 6500000000000000000,
				"epochInterval": 3153600,
				"decayFactor": 0.88,
				"rewardTaxRate": 0.5,
				"originalBlockNumber": 12345678,
				"stakingEnabled": true,
				"windowSize": 512,
				"stakePerBlock": 1000000000000000000000000,
				"poswStakingDecayEnabled": true,
				"diffDivider": 10000
			}}`,
			true,
		},
		{
			"quoted ratios",
			`{"posw": {
				"adjustmentCutoff": 7,
				"adjustmentFactor": 512,
				"minimumDifficulty": 100,
				"coinbaseAmount": 6500000000000000000,
				"epochInterval": 3153600,
				"decayFactor": "0.88",
				"rewardTaxRate": "0.5",
				"originalBlockNumber": 12345678,
				"stakingEnabled": true,
				"windowSize": 512,
				"stakePerBlock": 1000000000000000000000000,
				"poswStakingDecayEnabled": true,
				"diffDivider": 10000
			}}`,
			true,
		},
		{
			"quoted",
			`{"posw": {
				"adjustmentCutoff": "7",
				"adjustmentFactor": "512",
				"minimumDifficulty": "100",
				"coinbaseAmount": "6500000000000000000",
				"epochInterval": "3153600",
				"decayFactor": "0.88",
				"rewardTaxRate": "0.5",
				"originalBlockNumber": "12345678",
				"stakingEnabled": "true",
				"windowSize": "512",
				"stakePerBlock": "1000000000000000000000000",
				"poswStakingDecayEnabled": "true",
				"diffDivider": "10000"
			}}`,
			false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var config QuarkChainConfig
			err := json.Unmarshal([]byte(test.input), &config)
			if err == nil {
				err = config.PoSW.Check()
			}
			if (err == nil) != test.valid {
				t.Fatalf("config validation %v, want valid %v", err, test.valid)
			}
			if err != nil {
				return
			}
			if config.PoSW.CoinbaseAmount.String() != "6500000000000000000" || config.PoSW.StakePerBlock.String() != "1000000000000000000000000" {
				t.Fatal("mainnet integer fields changed value")
			}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				PoSW map[string]json.RawMessage `json:"posw"`
			}
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			wantFields := map[string]string{
				"adjustmentCutoff": "7", "adjustmentFactor": "512",
				"minimumDifficulty": "100", "coinbaseAmount": "6500000000000000000",
				"epochInterval": "3153600", "decayFactor": "0.88", "rewardTaxRate": "0.5",
				"originalBlockNumber": "12345678", "stakingEnabled": "true", "windowSize": "512",
				"stakePerBlock": "1000000000000000000000000", "poswStakingDecayEnabled": "true", "diffDivider": "10000",
			}
			if len(fields.PoSW) != len(wantFields) {
				t.Fatalf("unexpected JSON fields: %s", encoded)
			}
			for field, want := range wantFields {
				if string(fields.PoSW[field]) != want {
					t.Fatalf("%s: have %s, want JSON value %s", field, fields.PoSW[field], want)
				}
			}
			var roundtrip QuarkChainConfig
			if err := json.Unmarshal(encoded, &roundtrip); err != nil || !reflect.DeepEqual(config, roundtrip) {
				t.Fatalf("config JSON round trip: %s, %v", encoded, err)
			}
		})
	}
}

func TestPoSWConfigRatiosJSON(t *testing.T) {
	for _, field := range []string{"decayFactor", "rewardTaxRate"} {
		for _, test := range []struct {
			input string
			valid bool
		}{
			{`0.88`, true}, {`"0.88"`, true}, {`"22/25"`, false},
			// Values above one must not round down through float64.
			{`1.000000000000000000000000000000000001`, false},
			{`"1.000000000000000000000000000000000001"`, false},
		} {
			config := validPoSWConfig()
			err := json.Unmarshal([]byte(`{"`+field+`":`+test.input+`}`), config)
			if err == nil {
				err = config.Check()
			}
			if (err == nil) != test.valid {
				t.Fatalf("%s=%s: validation %v, want valid %v", field, test.input, err, test.valid)
			}
			if err != nil {
				continue
			}
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields[field]) != "0.88" {
				t.Fatalf("%s=%s: have %s, want unquoted 0.88", field, test.input, fields[field])
			}
		}
	}
}

func TestPoSWConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*PoSWConfig)
	}{
		{"zero cutoff", func(c *PoSWConfig) { c.AdjustmentCutoff = 0 }},
		{"zero factor", func(c *PoSWConfig) { c.AdjustmentFactor = 0 }},
		{"missing minimum", func(c *PoSWConfig) { c.MinimumDifficulty = nil }},
		{"zero minimum", func(c *PoSWConfig) { c.MinimumDifficulty = new(big.Int) }},
		{"negative minimum", func(c *PoSWConfig) { c.MinimumDifficulty.SetInt64(-1) }},
		{"missing subsidy", func(c *PoSWConfig) { c.CoinbaseAmount = nil }},
		{"negative subsidy", func(c *PoSWConfig) { c.CoinbaseAmount.SetInt64(-1) }},
		{"subsidy overflow", func(c *PoSWConfig) { c.CoinbaseAmount.Lsh(big.NewInt(1), 256) }},
		{"zero epoch", func(c *PoSWConfig) { c.EpochInterval = 0 }},
		{"missing decay", func(c *PoSWConfig) { c.DecayFactor = "" }},
		{"negative decay", func(c *PoSWConfig) { c.DecayFactor = "-0.5" }},
		{"decay above one", func(c *PoSWConfig) { c.DecayFactor = "1.5" }},
		{"decay denominator", func(c *PoSWConfig) { c.DecayFactor = "0.001" }},
		{"fraction is not JSON number", func(c *PoSWConfig) { c.DecayFactor = "22/25" }},
		{"missing tax", func(c *PoSWConfig) { c.RewardTaxRate = "" }},
		{"negative tax", func(c *PoSWConfig) { c.RewardTaxRate = "-0.5" }},
		{"tax above one", func(c *PoSWConfig) { c.RewardTaxRate = "1.5" }},
		{"invalid tax", func(c *PoSWConfig) { c.RewardTaxRate = "bad" }},
		{"zero window", func(c *PoSWConfig) { c.WindowSize = 0 }},
		{"zero divider", func(c *PoSWConfig) { c.DiffDivider = 0 }},
		{"missing stake", func(c *PoSWConfig) { c.StakePerBlock = nil }},
		{"zero stake", func(c *PoSWConfig) { c.StakePerBlock.SetUint64(0) }},
		{"negative stake", func(c *PoSWConfig) { c.StakePerBlock.SetInt64(-1) }},
		{"stake overflow", func(c *PoSWConfig) { c.StakePerBlock.Lsh(big.NewInt(1), 256) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := validPoSWConfig()
			test.change(config)
			if err := config.Check(); err == nil {
				t.Fatal("invalid consensus config accepted")
			}
			if err := (&ChainConfig{QuarkChain: &QuarkChainConfig{PoSW: config}}).CheckConfigForkOrder(); err == nil {
				t.Fatal("invalid shard consensus config accepted")
			}
		})
	}
	for _, config := range []*PoSWConfig{nil, {}} {
		chainConfig := *MergedTestChainConfig
		chainConfig.QuarkChain = &QuarkChainConfig{PoSW: config}
		if config.Check() == nil || chainConfig.CheckConfigForkOrder() == nil {
			t.Fatal("missing consensus parameters accepted")
		}
	}
	for _, factor := range []json.Number{"0", "0.5", "0.88", "0.99", "1"} {
		config := validPoSWConfig()
		config.DecayFactor = factor
		config.CoinbaseAmount.SetUint64(0)
		config.StakingEnabled = false
		config.StakePerBlock, config.WindowSize, config.DiffDivider = nil, 0, 0
		if err := config.Check(); err != nil {
			t.Fatalf("zero subsidy, staking disabled, decay %s: %v", factor, err)
		}
	}
}

func TestPoSWConfigAmountsJSON(t *testing.T) {
	const maxAmount = "115792089237316195423570985008687907853269984665640564039457584007913129639935"
	for _, field := range []string{"coinbaseAmount", "stakePerBlock"} {
		for _, test := range []struct {
			input string
			valid bool
		}{
			{"100", true}, {maxAmount, true}, {`"100"`, false}, {"0.88", false}, {"-1", false},
			{"115792089237316195423570985008687907853269984665640564039457584007913129639936", false},
		} {
			config := validPoSWConfig()
			err := json.Unmarshal([]byte(`{"`+field+`":`+test.input+`}`), config)
			if err == nil {
				err = config.Check()
			}
			if (err == nil) != test.valid {
				t.Fatalf("%s=%s: validation %v, want valid %v", field, test.input, err, test.valid)
			}
		}
	}
}

func TestPoSWConfigHeightQueries(t *testing.T) {
	for _, test := range []struct{ number, reward, stake uint64 }{
		{0, 0, 50}, {1, 12, 25}, {11, 6, 12}, {71, 0, 100},
	} {
		config := validPoSWConfig()
		config.DecayFactor = "0.5"
		config.OriginalBlockNumber = 19
		config.PoSWStakingDecayEnabled = true
		before, _ := json.Marshal(config)
		number := new(big.Int).SetUint64(test.number)
		reward, stake := config.BlockRewardAt(number), config.StakePerBlockAt(number)
		if reward.Uint64() != test.reward || stake.Uint64() != test.stake {
			t.Fatalf("height %d: reward %s stake %s, want %d/%d", test.number, reward, stake, test.reward, test.stake)
		}
		reward.SetInt64(-1)
		stake.SetInt64(-1)
		after, _ := json.Marshal(config)
		if string(before) != string(after) || number.Uint64() != test.number {
			t.Fatal("height queries or their returned integers modified inputs")
		}
		config.PoSWStakingDecayEnabled = false
		fixed := config.StakePerBlockAt(number)
		fixed.SetInt64(0)
		if config.StakePerBlock.Uint64() != 100 || config.StakePerBlockAt(number).Uint64() != 100 {
			t.Fatal("fixed stake query modified or decayed the configured value")
		}
	}
	config := validPoSWConfig()
	config.DecayFactor = "0.5"
	config.EpochInterval, config.OriginalBlockNumber = ^uint64(0), ^uint64(0)
	if got := config.BlockRewardAt(big.NewInt(1)); got.Uint64() != 25 {
		t.Fatalf("original height overflow: reward %s, want 25", got)
	}
}

func TestRegenesisRewards(t *testing.T) {
	for _, test := range []struct {
		number, offset uint64
		want           uint64
	}{
		{0, 19, 0}, {1, 18, 25}, {1, 19, 12}, {10, 19, 12}, {11, 19, 6}, {71, 19, 0},
	} {
		config := &PoSWConfig{
			CoinbaseAmount: big.NewInt(100), EpochInterval: 10, DecayFactor: "0.5", RewardTaxRate: "0.5",
			OriginalBlockNumber: test.offset,
		}
		if got := config.BlockRewardAt(new(big.Int).SetUint64(test.number)); got.Cmp(new(big.Int).SetUint64(test.want)) != 0 {
			t.Fatalf("height %d offset %d: %s want %d", test.number, test.offset, got, test.want)
		}
	}
}

func TestRewardDecay(t *testing.T) {
	// Expected rewards follow pyquarkchain: floor the epoch decay first,
	// then multiply by the local reward share and floor again.
	for _, test := range []struct {
		name   string
		factor string
		amount uint64
		epoch  uint64
		want   uint64
	}{
		{"before decay", "0.88", 100, 0, 50},
		{"first epoch", "0.88", 100, 1, 44},
		{"second epoch", "0.88", 100, 2, 38},
		{"round once after exponentiation", "0.88", 100, 3, 34},
		{"slow decay", "0.88", 100, 6, 23},
		{"decay before tax", "0.88", 3, 1, 1},
		{"denominator limit", "0.99", 100, 100, 18},
		{"fully decayed", "0.88", 100, 100, 0},
		{"no decay", "1", 100, 100, 50},
		{"zero factor before decay", "0", 100, 0, 50},
		{"zero factor after decay", "0", 100, 1, 0},
		{"zero subsidy", "0.88", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := &PoSWConfig{
				AdjustmentCutoff: 7, AdjustmentFactor: 512, MinimumDifficulty: big.NewInt(1),
				CoinbaseAmount: new(big.Int).SetUint64(test.amount), EpochInterval: 1,
				DecayFactor: json.Number(test.factor), RewardTaxRate: "0.5",
				OriginalBlockNumber: test.epoch,
			}
			if err := config.Check(); err != nil {
				t.Fatal(err)
			}
			// A positive new block number with epoch zero is needed to exercise
			// the subsidy before its first decay without crediting genesis.
			if test.epoch == 0 {
				config.EpochInterval = 2
			} else {
				config.OriginalBlockNumber--
			}
			if got := config.BlockRewardAt(big.NewInt(1)); got.Cmp(new(big.Int).SetUint64(test.want)) != 0 {
				t.Fatalf("reward %s, want %d", got, test.want)
			}
			if config.DecayFactor.String() != test.factor {
				t.Fatalf("decay factor mutated: %s", config.DecayFactor)
			}
		})
	}
}
