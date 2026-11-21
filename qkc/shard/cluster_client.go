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

package shard

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// ShardBackend supplies the concurrent-safe shard operations served by the
// cluster client. Initialization must finish before mining and block imports.
type ShardBackend interface {
	Init(*qkctypes.RootBlock) error
	AddRootBlock(*qkctypes.RootBlock) (bool, error)
	GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error)
	GetUnconfirmedHeaderList() ([]*types.Header, error)
	AddXshardTxList(common.Hash, []*types.DepositTx) error
}

// ClusterClient is implemented by the component that owns the cluster
// connections. A Shard holds this narrow interface instead of the connections
// or wire-message encoders.
// Implementations must support concurrent calls and honor context cancellation and deadlines.
type ClusterClient interface {
	// Start binds the backend and starts communication without waiting for Init.
	Start(backend ShardBackend) error
	// Stop closes connections and drains handlers before shard resources close.
	Stop() error

	// BroadcastXShardTxList delivers depositTxList from shardBlockHash to the
	// hosts serving chainID. An empty transaction list still advances that
	// chain's inbox cursor. A send failure, a nonzero remote ErrorCode
	// (returned as *clusternet.RemoteError), or no connection for chainID is returned
	// as an error. If only some hosts acknowledge the request, the caller must
	// not report the block header to Master and must retry delivery to chainID.
	BroadcastXShardTxList(ctx context.Context, chainID *big.Int, shardBlockHash common.Hash, depositTxList []*types.DepositTx) error

	// SendShardBlockHeaderToMaster reports an accepted shard block to Master.
	// It returns nil after a completed RPC whose remote ErrorCode is zero. A
	// nonzero Master ErrorCode is returned as *clusternet.RemoteError; a transport or
	// RPC failure is returned as is.
	SendShardBlockHeaderToMaster(ctx context.Context, blockHeader *types.Header, coinbaseAmount *uint256.Int) error
}
