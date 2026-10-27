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

// Package master defines the outbound contract used by master business logic.
package master

import (
	"context"

	"github.com/ethereum/go-ethereum/qkc/wire"
)

// MasterSender is implemented by the component that owns the connections to
// chains. Master business logic holds this interface instead of those
// connections.
type MasterSender interface {
	// BroadcastAddRootBlockToChains sends req to every connected chain and waits
	// for all acknowledgements.
	BroadcastAddRootBlockToChains(ctx context.Context, req *wire.AddRootBlockRequest) error

	// SendGetRootChainStakesFromChain asks chainID for the staking information
	// for req and returns that chain's response.
	SendGetRootChainStakesFromChain(ctx context.Context, chainID uint32, req *wire.GetRootChainStakesRequest) (*wire.GetRootChainStakesResponse, error)
}
