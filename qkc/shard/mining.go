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
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/qkc/mining"
)

var _ mining.MinerBackend = (*Shard)(nil)

// BuildMiningWork packages a shard candidate and its adjusted seal difficulty.
func (s *Shard) BuildMiningWork(ctx context.Context, coinbase common.Address) (*mining.Candidate, error) {
	block, difficulty, err := s.CreateBlockToMine(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	header := block.Header()
	sealHash, err := header.QKCSealHash()
	if err != nil {
		return nil, err
	}
	extra, err := header.QKCExtra()
	if err != nil {
		return nil, err
	}
	return &mining.Candidate{
		MiningWork: mining.MiningWork{
			SealHash:   sealHash,
			Number:     block.NumberU64(),
			Difficulty: difficulty,
			Time:       block.Time(),
		},
		Parent: block.ParentHash(),
		Anchor: extra.PrevRootBlock,
		Submit: func(nonce types.BlockNonce, mixHash common.Hash) error {
			sealedHeader, sealedExtra := types.CopyHeader(header), *extra
			sealedExtra.Nonce, sealedExtra.MixHash = nonce, mixHash
			sealedHeader.Extra = sealedExtra.Encode()
			return s.InsertMinedBlock(block.WithSeal(sealedHeader))
		},
	}, nil
}

// MiningHead uses the root tip as the anchor so a root reorg refreshes work
// even when the shard head stays on the same block.
func (s *Shard) MiningHead() (common.Hash, common.Hash, error) {
	return s.chain.headAndRootTip()
}

// SubscribeMiningHead forwards changes to the shard head as wakeups.
func (s *Shard) SubscribeMiningHead(ch chan<- struct{}) event.Subscription {
	heads := make(chan core.ChainHeadEvent, 1)
	sub := s.SubscribeChainHeadEvent(heads)
	if sub == nil {
		return nil
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case <-heads:
				select {
				case ch <- struct{}{}:
				default:
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	})
}
