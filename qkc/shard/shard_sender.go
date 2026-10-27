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

// Package shard defines the outbound contract used by shard business logic.
package shard

import (
	"context"

	"github.com/ethereum/go-ethereum/qkc/wire"
)

// ShardSender is implemented by ShardNode. A Shard holds this narrow interface
// instead of MasterConn or the shard connection pool.
type ShardSender interface {
	// BroadcastXShardTxList sends req to every connection serving req.ChainID and
	// waits for their acknowledgements.
	BroadcastXShardTxList(ctx context.Context, req *wire.AddXshardTxListRequest) error

	// SendMinorBlockHeaderToMaster reports an accepted shard block to the master.
	// It returns the master's response after the RPC completes.
	SendMinorBlockHeaderToMaster(ctx context.Context, req *wire.AddMinorBlockHeaderRequest) (*wire.AddMinorBlockHeaderResponse, error)
}
