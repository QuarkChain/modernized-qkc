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
	"github.com/ethereum/go-ethereum/log"
	"github.com/holiman/uint256"
)

// MockClusterClient only logs delivery calls; it neither opens connections nor
// initializes the shard. Replace it when the real SlaveService is integrated.
// TODO(mock): Add mock xshard deposits and root blocks here, forwarding them to
// Shard.AddXshardTxList and Shard.Init/AddRootBlock through the bound reference.
type MockClusterClient struct {
	Shard *Shard // Bound by Ethereum before any requests are handled.
}

var _ ClusterClient = (*MockClusterClient)(nil)

func (*MockClusterClient) BroadcastXShardTxList(_ context.Context, chainID *big.Int, hash common.Hash, deposits []*types.DepositTx) error {
	log.Info("Mock cross-shard delivery", "chain", chainID, "block", hash, "deposits", len(deposits))
	return nil
}

func (*MockClusterClient) SendMinorBlockHeaderToMaster(_ context.Context, header *types.Header, amount *uint256.Int) error {
	log.Info("Mock shard header report", "block", header.Hash(), "coinbaseAmount", amount)
	return nil
}
