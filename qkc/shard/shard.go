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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/miner"
)

// MinerBackend is the shard layer used by PoWMiner, like goquarkchain's
// MinerAPI. Mined blocks take the same path as blocks from the network.
type MinerBackend interface {
	// CreateBlockToMine builds an unsealed candidate on the shard head, paying
	// coinbase.
	CreateBlockToMine(ctx context.Context, coinbase common.Address) (*types.Block, error)

	// InsertMinedBlock imports a sealed candidate.
	InsertMinedBlock(block *types.Block) error

	// CurrentBlock returns the shard head.
	CurrentBlock() *types.Header

	// SubscribeChainHeadEvent registers a subscription for shard head changes.
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
}

var _ MinerBackend = (*Shard)(nil)

// Shard is the shard layer on top of a ShardChain, handling the cluster side of
// block production.
type Shard struct {
	chain *ShardChain
}

// NewShard creates the shard layer for chain.
func NewShard(chain *ShardChain) *Shard {
	return &Shard{chain: chain}
}

// CreateBlockToMine builds a candidate on the shard head with the chain's
// builder. Like pyquarkchain, its timestamp may run up to a second ahead of the
// clock to follow the parent.
func (s *Shard) CreateBlockToMine(ctx context.Context, coinbase common.Address) (*types.Block, error) {
	parent := s.chain.chain.CurrentBlock()
	return s.chain.BuildBlock(ctx, miner.QKCBlockArgs{
		Parent:       parent.Hash(),
		Timestamp:    max(uint64(time.Now().Unix()), parent.Time+1),
		FeeRecipient: coinbase,
	})
}

// InsertMinedBlock imports a block sealed for this shard.
//
// TODO(#21): like a block from the network, also broadcast it, send its xshard
// deposits to neighbor shards and then report it to master.
func (s *Shard) InsertMinedBlock(block *types.Block) error {
	return s.chain.InsertShardBlock(block)
}

// CurrentBlock returns the shard head.
func (s *Shard) CurrentBlock() *types.Header {
	return s.chain.chain.CurrentBlock()
}

// SubscribeChainHeadEvent registers a subscription for shard head changes.
func (s *Shard) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return s.chain.chain.SubscribeChainHeadEvent(ch)
}
