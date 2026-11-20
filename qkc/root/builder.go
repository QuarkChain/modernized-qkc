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

// RootBlockBuilder builds candidates using the root chain's consensus engine and shard reports.
type RootBlockBuilder struct {
	chain         poswroot.RootChainReader
	engine        poswroot.RootEngine
	clusterClient ClusterClient
	chainIDs      []*big.Int
	maxPerShard   uint64
}

// NewRootBlockBuilder binds the engine and its network configuration. The cluster
// client is optional when the caller always supplies ShardHeaders.
func NewRootBlockBuilder(config *params.QuarkChainConfig, chain poswroot.RootChainReader, engine poswroot.RootEngine, clusterClient ClusterClient) (*RootBlockBuilder, error) {
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
// Each shard's new headers may reference only the parent root or its ancestors.
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
	isAncestor := poswroot.NewRootAncestorCheck(ctx, builder.chain, args.Parent)
	for _, chainID := range builder.chainIDs {
		key := chainID.String()
		if err := builder.appendShardHeaders(ctx, block, chainID, confirmed[key], candidates[key], isAncestor); err != nil {
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
func (builder *RootBlockBuilder) appendShardHeaders(ctx context.Context, block *qkctypes.RootBlock, chainID *big.Int, confirmed common.Hash, headers []*types.Header, isAncestor func(common.Hash) (bool, error)) error {
	if confirmed == (common.Hash{}) {
		return errors.New("missing shard confirmation at the parent root")
	}
	// Start at a direct child of the parent's confirmation, or scan for the
	// confirmed header by hash and discard the prefix through it.
	var previousHash common.Hash
	if len(headers) > 0 && headers[0] != nil {
		previousHash = headers[0].ParentHash
	}
	skipped := 0
	for skipped < len(headers) && previousHash != confirmed {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := headers[skipped]
		if header == nil || header.Number == nil || header.Number.Sign() < 0 {
			return fmt.Errorf("missing header or invalid height at index %d", skipped)
		}
		previousHash = header.Hash()
		skipped++
	}

	pending := headers[skipped:]
	limit := min(uint64(len(pending)), builder.maxPerShard)
	for offset, header := range pending[:limit] {
		if err := ctx.Err(); err != nil {
			return err
		}
		index := skipped + offset
		if err := checkShardHeaderHeight(headers, index); err != nil {
			return err
		}
		if header.Time > block.Time() {
			break
		}
		hash := header.Hash()
		amount, err := builder.chain.GetShardBlockCoinbaseAmount(hash)
		if err != nil {
			return fmt.Errorf("read report for %s: %w", hash, err)
		}
		if amount == nil {
			break
		}
		extra, err := validateShardHeaderLink(header, previousHash, chainID)
		if err != nil {
			return err
		}
		sameChain, err := isAncestor(extra.PrevRootBlock)
		if err != nil {
			return err
		}
		if !sameChain {
			break
		}
		block.AddShardBlockHeader(header)
		previousHash = hash
	}
	return nil
}

func checkShardHeaderHeight(headers []*types.Header, index int) error {
	header := headers[index]
	if header == nil || header.Number == nil || header.Number.Sign() <= 0 {
		return fmt.Errorf("missing header or invalid height at index %d", index)
	}
	if index > 0 {
		previous := headers[index-1].Number
		next := new(big.Int).Add(previous, common.Big1)
		if header.Number.Cmp(next) != 0 {
			return fmt.Errorf("nonconsecutive shard heights %v and %v", previous, header.Number)
		}
	}
	return nil
}

func validateShardHeaderLink(header *types.Header, parentHash common.Hash, chainID *big.Int) (*types.QKCExtra, error) {
	if header.ParentHash != parentHash {
		return nil, fmt.Errorf("header %s at height %v does not extend %s", header.Hash(), header.Number, parentHash)
	}
	extra, err := header.QKCExtra()
	if err != nil {
		return nil, err
	}
	if extra.ChainID.Cmp(chainID) != 0 {
		return nil, fmt.Errorf("header %s belongs to shard %v", header.Hash(), extra.ChainID)
	}
	return extra, nil
}
