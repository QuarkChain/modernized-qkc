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

	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// accumulateRewards credits the shard's subsidy share, without uncle rewards or
// transaction fees. Like goquarkchain, split the configured subsidy before decay.
// OriginalBlockNumber continues the old chain's schedule across regenesis.
func accumulateRewards(config *params.QuarkChainConfig, state vm.StateDB, header *types.Header) {
	if header.Number.Sign() == 0 {
		return
	}
	c := config.Reward
	localRate := new(big.Rat).Sub(big.NewRat(1, 1), c.RewardTaxRate)
	reward := new(big.Int).Mul(c.CoinbaseAmount.ToBig(), localRate.Num())
	reward.Div(reward, localRate.Denom())
	height := new(big.Int).Add(header.Number, new(big.Int).SetUint64(config.OriginalBlockNumber))
	epoch := height.Div(height, new(big.Int).SetUint64(c.EpochInterval))
	if c.DecayDivisor > 1 {
		// Every epoch divides by at least two. Avoid constructing enormous powers
		// once the integer subsidy is already guaranteed to be zero.
		if epoch.Cmp(big.NewInt(int64(reward.BitLen()))) >= 0 {
			reward.SetInt64(0)
		} else {
			divisor := new(big.Int).Exp(new(big.Int).SetUint64(c.DecayDivisor), epoch, nil)
			reward.Div(reward, divisor)
		}
	}
	amount, _ := uint256.FromBig(reward)
	state.AddBalance(header.Coinbase, amount, tracing.BalanceIncreaseRewardMineBlock)
}
