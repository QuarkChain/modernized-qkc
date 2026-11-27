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

package root

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/qkc/mining"
)

var _ mining.MinerBackend = (*RootChain)(nil)

// BuildMiningWork serves the header's base difficulty and the discount
// available to the authorized signer on this candidate's parent branch.
func (c *RootChain) BuildMiningWork(ctx context.Context, coinbase common.Address) (*mining.Candidate, error) {
	block, err := c.CreateBlockToMine(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	header := block.Header()
	info, err := poswroot.GetPoSWInfo(c.config, rootStakeReader{c, ctx}, header)
	if err != nil {
		return nil, err
	}
	var divider uint64
	if info.EligibleForDiscount() {
		divider = c.config.PoSW.DiffDivider
	}
	return &mining.Candidate{
		MiningWork: mining.MiningWork{
			SealHash:    c.engine.SealHash(header),
			Number:      block.NumberU64(),
			Difficulty:  header.Difficulty.ToBig(),
			DiffDivider: divider,
			Time:        header.Time,
		},
		Parent: header.ParentHash,
		Submit: func(nonce types.BlockNonce, mixHash common.Hash, signature *[65]byte) error {
			return c.insertBlock(block.WithMiningResult(nonce.Uint64(), mixHash, signature), true)
		},
	}, nil
}

// MiningHead returns the canonical root tip, or zero while the chain is unavailable.
func (c *RootChain) MiningHead() common.Hash {
	if !c.Ready() {
		return common.Hash{}
	}
	return c.RootTip().Hash()
}

// MiningAnchor returns zero because root work depends only on the root head.
func (c *RootChain) MiningAnchor() common.Hash {
	return common.Hash{}
}

// SubscribeMiningHead signals changes to the canonical root tip.
func (c *RootChain) SubscribeMiningHead(ch chan<- struct{}) event.Subscription {
	return mining.ForwardHeadEvents(c.SubscribeRootHeadEvent, ch)
}
