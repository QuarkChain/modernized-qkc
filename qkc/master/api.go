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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// API is the public JSON-RPC adapter registered with node under "master".
// It serves the same role as geth's internal/ethapi RPC services.
// It delegates root queries to APIBackend and converts results to RPC JSON;
// missing records become JSON null. Import and rewind methods on APIBackend
// are not exposed by this service yet.
type API struct {
	backend *APIBackend
}

// GetHeaderByNumber returns a canonical root header as RPC JSON.
func (api *API) GetHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (map[string]interface{}, error) {
	header, err := api.backend.HeaderByNumber(ctx, number)
	if err != nil || header == nil {
		return nil, err
	}
	return rpcMarshalRootHeader(header), nil
}

// GetHeaderByHash returns a root header from any stored branch as RPC JSON.
func (api *API) GetHeaderByHash(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	header, err := api.backend.HeaderByHash(ctx, hash)
	if err != nil || header == nil {
		return nil, err
	}
	return rpcMarshalRootHeader(header), nil
}

// GetBlockByNumber returns a canonical root block, including its body, as RPC JSON.
func (api *API) GetBlockByNumber(ctx context.Context, number rpc.BlockNumber) (map[string]interface{}, error) {
	block, err := api.backend.BlockByNumber(ctx, number)
	if err != nil || block == nil {
		return nil, err
	}
	return rpcMarshalRootBlock(block), nil
}

// GetBlockByHash returns a root block from any stored branch as RPC JSON.
func (api *API) GetBlockByHash(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	block, err := api.backend.BlockByHash(ctx, hash)
	if err != nil || block == nil {
		return nil, err
	}
	return rpcMarshalRootBlock(block), nil
}

// rpcMarshalRootHeader keeps RootBlockHeader's JSON field names and uses geth's
// hex encoders for quantities and byte strings, without Python legacy aliases.
func rpcMarshalRootHeader(header *types.RootBlockHeader) map[string]interface{} {
	return map[string]interface{}{
		"version":          hexutil.Uint64(header.Version),
		"number":           hexutil.Uint64(header.Number),
		"hash":             header.Hash(),
		"parentHash":       header.ParentHash,
		"shardHeaderHash":  header.ShardHeaderHash,
		"miner":            header.Coinbase,
		"coinbaseAmount":   hexutil.U256(header.CoinbaseAmount),
		"timestamp":        hexutil.Uint64(header.Time),
		"difficulty":       hexutil.U256(header.Difficulty),
		"total_difficulty": hexutil.U256(header.TotalDifficulty),
		"extraData":        hexutil.Bytes(header.Extra),
		"nonce":            hexutil.Uint64(header.Nonce),
		"mixHash":          header.MixDigest,
		"signature":        hexutil.Bytes(header.Signature[:]),
	}
}

// rpcMarshalRootBlock adds shard headers, tracking data and SSZ size to the
// header fields. Empty shard header lists are encoded as [] rather than null.
func rpcMarshalRootBlock(block *types.RootBlock) map[string]interface{} {
	result := rpcMarshalRootHeader(block.Header())
	headers := block.ShardBlockHeaders()
	if headers == nil {
		headers = make(types.ShardBlockHeaders, 0)
	}
	// Shard headers already implement geth's JSON encoding, including their hash.
	result["shardBlockHeaders"] = headers
	result["trackingData"] = hexutil.Bytes(block.TrackingData())
	result["size"] = hexutil.Uint64(block.Size())
	return result
}
