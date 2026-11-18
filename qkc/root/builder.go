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

// Package root builds candidates for the QuarkChain root chain.
package root

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// RootBlockArgs selects the parent and coinbase for one unsealed root block.
// A zero timestamp selects the later of the current time and the parent time plus one.
// ShardHeaders maps decimal chain IDs to lists in ascending height order. Nil
// collects the lists from the cluster; an empty map builds a root without shard headers.
type RootBlockArgs struct {
	Parent       *qkctypes.RootBlockHeader
	Coinbase     common.Address
	Timestamp    uint64
	ShardHeaders map[string][]*types.Header
}

// ShardHeaderSource provides unconfirmed headers in ascending height order per shard.
// A collection failure must be returned as an error, even if some shards responded.
type ShardHeaderSource interface {
	GetUnconfirmedHeaderList(ctx context.Context) (map[string][]*types.Header, error)
}

// RootBlockBuilder builds candidates using the root chain's consensus engine and shard reports.
type RootBlockBuilder struct {
	chain         poswroot.RootChainReader
	engine        poswroot.RootEngine
	clusterClient ShardHeaderSource
	chainIDs      []*big.Int
	maxPerShard   uint64
}

// NewRootBlockBuilder binds the engine and its network configuration. The cluster
// client is optional when the caller always supplies ShardHeaders.
func NewRootBlockBuilder(config *params.QuarkChainConfig, chain poswroot.RootChainReader, engine poswroot.RootEngine, clusterClient ShardHeaderSource) (*RootBlockBuilder, error) {
	if err := config.Check(); err != nil {
		return nil, err
	}
	if chain == nil || engine == nil {
		return nil, errors.New("root block builder requires a chain reader and engine")
	}
	chainIDs := slices.Clone(config.ChainIDs)
	slices.SortFunc(chainIDs, (*big.Int).Cmp)
	return &RootBlockBuilder{
		chain: chain, engine: engine, clusterClient: clusterClient,
		chainIDs: chainIDs, maxPerShard: config.MaxBlocksPerRootBlock,
	}, nil
}

// BuildBlock returns a finalized, unsealed root candidate on the selected parent.
// Since shards collect headers at their own root tips, the builder skips headers
// already confirmed by the parent and omits linked lists that do not connect to it.
// The caller must still ensure the selected headers reference the parent's root branch.
// Signing, sealing and importing the result belong to the caller.
func (builder *RootBlockBuilder) BuildBlock(ctx context.Context, args RootBlockArgs) (*qkctypes.RootBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args.Parent == nil {
		return nil, consensus.ErrUnknownAncestor
	}
	if args.Parent.Number == math.MaxUint32 {
		return nil, consensus.ErrInvalidNumber
	}
	candidates := args.ShardHeaders
	if candidates == nil {
		if builder.clusterClient == nil {
			return nil, errors.New("collecting shard headers requires a cluster client")
		}
		headers, err := builder.clusterClient.GetUnconfirmedHeaderList(ctx)
		if err != nil {
			return nil, err
		}
		candidates = headers
	}
	timestamp := args.Timestamp
	if timestamp == 0 {
		timestamp = max(uint64(time.Now().Unix()), args.Parent.Time+1)
	}
	header := &qkctypes.RootBlockHeader{
		ParentHash: args.Parent.Hash(),
		Number:     args.Parent.Number + 1,
		Time:       timestamp,
		Coinbase:   args.Coinbase,
	}
	if err := builder.engine.Prepare(builder.chain, header); err != nil {
		return nil, err
	}
	confirmed, err := builder.chain.GetLastConfirmedShardBlocks(header.ParentHash)
	if err != nil {
		return nil, err
	}
	block := qkctypes.NewRootBlockWithHeader(header)
	for _, chainID := range builder.chainIDs {
		key := chainID.String()
		if err := builder.appendShardHeaders(ctx, block, chainID, confirmed[key], candidates[key]); err != nil {
			return nil, fmt.Errorf("shard %s: %w", key, err)
		}
	}
	block, err = builder.engine.Finalize(builder.chain, block)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return block, nil
}

// appendShardHeaders counts only new headers against the per-shard limit.
// An unreported header or one newer than the root defers its descendants too.
func (builder *RootBlockBuilder) appendShardHeaders(ctx context.Context, block *qkctypes.RootBlock, chainID *big.Int, confirmed common.Hash, headers []*types.Header) error {
	if len(headers) == 0 {
		return nil
	}
	if confirmed == (common.Hash{}) {
		return errors.New("missing shard confirmation at the parent root")
	}
	aligned := headers[0] != nil && headers[0].ParentHash == confirmed
	var previousHash common.Hash
	var added uint64
	for index, header := range headers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if header == nil || header.Number == nil || header.Number.Sign() < 0 || (aligned && header.Number.Sign() == 0) {
			return fmt.Errorf("missing header or invalid height at index %d", index)
		}
		if index > 0 && header.Number.Cmp(new(big.Int).Add(headers[index-1].Number, common.Big1)) != 0 {
			return fmt.Errorf("nonconsecutive shard heights %v and %v", headers[index-1].Number, header.Number)
		}
		if aligned && header.Time > block.Time() {
			break
		}
		hash := header.Hash()
		if aligned {
			amount, err := builder.chain.GetShardBlockCoinbaseAmount(hash)
			if err != nil {
				return fmt.Errorf("read report for %s: %w", hash, err)
			}
			if amount == nil {
				break
			}
		}
		if index > 0 && header.ParentHash != previousHash {
			return fmt.Errorf("header %s at height %v does not extend %s", hash, header.Number, previousHash)
		}
		extra, err := header.QKCExtra()
		if err != nil {
			return err
		}
		if extra.ChainID.Cmp(chainID) != 0 {
			return fmt.Errorf("header %s belongs to shard %v", hash, extra.ChainID)
		}
		previousHash = hash
		if !aligned {
			aligned = hash == confirmed
			continue
		}
		block.AddShardBlockHeader(header)
		added++
		if added == builder.maxPerShard {
			break
		}
	}
	return nil
}
