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

package master

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// APIBackend adapts Backend's RootChain for API and internal master components.
// It returns root types and checks query cancellation and import readiness;
// RootChain owns validation, storage and shard delivery. API handles RPC JSON.
// APIBackend itself is not registered as an RPC service.
// Returned blocks and the latest header are shared and must not be modified.
type APIBackend struct {
	master *Backend
}

// HeaderByNumber resolves latest, earliest or a canonical uint32 height, even
// during cluster initialization. Missing data returns nil; pending, safe,
// finalized and out-of-range heights return an error.
func (b *APIBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.RootBlockHeader, error) {
	if err := b.checkContext(ctx); err != nil {
		return nil, err
	}
	switch number {
	case rpc.LatestBlockNumber:
		return b.master.chain.RootTip(), nil
	case rpc.EarliestBlockNumber:
		return b.master.chain.GetRootHeaderByNumber(0)
	default:
		if number < 0 || uint64(number) > math.MaxUint32 {
			return nil, fmt.Errorf("unsupported root block number: %s", number)
		}
		return b.master.chain.GetRootHeaderByNumber(uint32(number))
	}
}

// HeaderByHash returns a header copy from any stored branch, or nil if absent.
func (b *APIBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.RootBlockHeader, error) {
	if err := b.checkContext(ctx); err != nil {
		return nil, err
	}
	return b.master.chain.GetRootHeader(hash)
}

// BlockByNumber resolves latest, earliest or a canonical uint32 height, returning
// nil if absent. Latest uses one tip snapshot so a reorg cannot change its hash.
func (b *APIBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.RootBlock, error) {
	if err := b.checkContext(ctx); err != nil {
		return nil, err
	}
	switch number {
	case rpc.LatestBlockNumber:
		header := b.master.chain.RootTip()
		return b.master.chain.GetRootBlock(header.Hash())
	case rpc.EarliestBlockNumber:
		return b.master.chain.GetRootBlockByNumber(0)
	default:
		if number < 0 || uint64(number) > math.MaxUint32 {
			return nil, fmt.Errorf("unsupported root block number: %s", number)
		}
		return b.master.chain.GetRootBlockByNumber(uint32(number))
	}
}

// BlockByHash returns a shared, read-only block from any stored branch, or nil
// if absent. It does not require cluster readiness.
func (b *APIBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.RootBlock, error) {
	if err := b.checkContext(ctx); err != nil {
		return nil, err
	}
	return b.master.chain.GetRootBlock(hash)
}

// checkContext rejects queries canceled by the caller or backend shutdown.
func (b *APIBackend) checkContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.master.ctx.Err()
}

// InsertMinedBlock requires readiness, then delegates validation, persistence
// and shard delivery to RootChain.
// Imports use the backend context: shutdown cancels delivery, while a caller
// disappearing must not interrupt a block already committed locally.
func (b *APIBackend) InsertMinedBlock(block *types.RootBlock) error {
	if err := b.master.ctx.Err(); err != nil {
		return err
	}
	if !b.master.Ready() {
		return errors.New("master backend is not ready")
	}
	return b.master.chain.InsertBlock(block)
}

// SetHead preserves RootChain's unsupported rewind contract.
func (b *APIBackend) SetHead(number uint32) error {
	return b.master.chain.SetHead(number)
}
