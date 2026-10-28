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

// Package master defines the cluster client used by master business logic.
package master

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// ClusterClient is implemented by the component that owns the connections to
// chains. Master business logic holds this interface instead of those
// connections and does not construct wire messages.
type ClusterClient interface {
	// BroadcastRootBlock sends block to every connected ShardNode and waits for
	// all acknowledgements. A send failure or a nonzero remote ErrorCode is
	// returned as an error. If only some hosts acknowledge the block, the caller
	// must retain it as uncommitted and retry the complete broadcast.
	BroadcastRootBlock(ctx context.Context, block *qkctypes.RootBlock, expectSwitch bool) error

	// GetRootChainStakes asks chainID for the stakes recorded for address at
	// minorBlockHash. Its result is the decoded business response after a
	// completed RPC. A nonzero result ErrorCode reports a remote business
	// failure; err reports a transport or RPC failure.
	GetRootChainStakes(ctx context.Context, chainID uint32, address common.Address, minorBlockHash common.Hash) (*RootChainStakesResult, error)
}

// RootChainStakesResult is the business representation of a chain's staking
// response, separate from wire.GetRootChainStakesResponse. Stakes is decoded
// from the wire uint256 representation. ErrorCode reports the remote business
// result and does not indicate a transport failure.
type RootChainStakesResult struct {
	ErrorCode uint32
	Stakes    *uint256.Int
	Signer    common.Address
}
