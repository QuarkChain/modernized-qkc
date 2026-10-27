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

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// commitXShardDeposits executes the mandatory prefix selected by core.
func (miner *Miner) commitXShardDeposits(ctx context.Context, env *environment, deposits core.XShardDeposits) error {
	extra, err := env.header.QKCExtra()
	if err != nil {
		return err
	}
	extra.XShardGasLimit = env.header.GasLimit / params.XShardGasLimitDivisor
	extra.XShardGasUsed, extra.Cursor, err = core.ProcessXShardDeposits(ctx, deposits, extra.XShardGasLimit, func(deposit *types.DepositTx) (uint64, error) {
		tx := types.NewTx(deposit)
		if !env.txFitsSize(tx) {
			return 0, errors.New("mandatory cross-shard deposits exceed max block size")
		}
		env.state.SetTxContext(tx.Hash(), env.tcount, uint32(env.tcount+1))
		if err := miner.commitTransaction(ctx, env, tx); err != nil {
			return 0, err
		}
		return env.receipts[len(env.receipts)-1].GasUsed, nil
	})
	if err != nil {
		return err
	}
	env.header.Extra = extra.Encode()
	return nil
}
