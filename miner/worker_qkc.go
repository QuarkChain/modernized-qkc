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

package miner

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// commitXShardDeposits fills the mandatory prefix using the same stopping rule
// as core.BlockValidator.validateXShardDeposits.
func (miner *Miner) commitXShardDeposits(ctx context.Context, env *environment, deposits core.XShardDeposits) error {
	extra, err := env.header.QKCExtra()
	if err != nil {
		return err
	}
	extra.XShardGasLimit = env.header.GasLimit / params.XShardGasLimitDivisor
	extra.XShardGasUsed = 0
	extra.Cursor = deposits.Cursor()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Probe even when the budget is exhausted: EOF advances the cursor,
		// while a deposit that does not fit leaves it on the last included one.
		deposit, err := deposits.Next()
		if err != nil {
			return err
		}
		if deposit == nil {
			// Record the EOF cursor; subsequent Next calls leave it unchanged.
			extra.Cursor = deposits.Cursor()
			break
		}
		if deposit.Gas > extra.XShardGasLimit-extra.XShardGasUsed {
			break
		}
		tx := types.NewTx(deposit)
		if !env.txFitsSize(tx) {
			return errors.New("mandatory cross-shard deposits exceed max block size")
		}
		env.state.SetTxContext(tx.Hash(), env.tcount, uint32(env.tcount+1))
		if err := miner.commitTransaction(ctx, env, tx); err != nil {
			return err
		}
		// Admission uses declared gas; accounting uses execution receipts.
		gasUsed := env.receipts[len(env.receipts)-1].GasUsed
		if gasUsed > extra.XShardGasLimit-extra.XShardGasUsed {
			return fmt.Errorf("%w: deposit used %d with %d left", core.ErrXShardGas, gasUsed, extra.XShardGasLimit-extra.XShardGasUsed)
		}
		extra.XShardGasUsed += gasUsed
		extra.Cursor = deposits.Cursor()
	}
	env.header.Extra = extra.Encode()
	return nil
}
