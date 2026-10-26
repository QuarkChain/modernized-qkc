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

package shard

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"
)

// BuildBlock creates one unsealed shard candidate. Zero Parent and PrevRootBlock
// select the current shard head and root tip, respectively. These references are
// fixed before execution, so a concurrent head update cannot change the candidate.
// Pool transactions follow mandatory deposits; NoTxs skips only pool transactions.
// The caller owns pool and is responsible for sealing and importing the result.
func (c *ShardChain) BuildBlock(ctx context.Context, pool *txpool.TxPool, config miner.Config, args miner.QKCBlockArgs) (*types.Block, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Select both tips under the lock shared with head updates.
	c.mu.Lock()
	parent := c.chain.CurrentBlock()
	if args.Parent != (common.Hash{}) {
		parent = c.chain.GetHeaderByHash(args.Parent)
		if parent == nil {
			c.mu.Unlock()
			return nil, consensus.ErrUnknownAncestor
		}
	}
	args.Parent = parent.Hash()
	if args.PrevRootBlock == (common.Hash{}) {
		root := c.rootTip()
		if root == nil {
			c.mu.Unlock()
			return nil, ErrRootChainUninitialized
		}
		args.PrevRootBlock = root.Hash()
	}
	c.mu.Unlock()
	header := &types.Header{Extra: (&types.QKCExtra{PrevRootBlock: args.PrevRootBlock}).Encode()}
	if _, err := c.validateRootReference(header, parent); err != nil {
		return nil, err
	}
	builder := miner.New(miningBackend{c.chain, pool}, config, c.chain.Engine())
	return builder.BuildQKCBlock(ctx, args)
}

type miningBackend struct {
	chain *core.BlockChain
	pool  *txpool.TxPool
}

func (b miningBackend) BlockChain() *core.BlockChain { return b.chain }
func (b miningBackend) TxPool() *txpool.TxPool       { return b.pool }
