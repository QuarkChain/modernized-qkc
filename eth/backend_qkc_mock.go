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

package eth

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/clusternet/slavenet"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/holiman/uint256"
)

// mockClusterClient logs delivery calls for standalone tools and tests. It
// neither opens connections nor initializes the shard.
// TODO(mock): Add mock xshard deposits and root blocks here, forwarding them to
// Shard.AddXshardTxList and Shard.Init/AddRootBlock through the bound reference.
type mockClusterClient struct {
	Shard *shard.Shard // Bound by Start before any requests are handled.
}

var _ clusterClient = (*mockClusterClient)(nil)

func (c *mockClusterClient) Start(backend slavenet.ShardBackend) error {
	c.Shard, _ = backend.(*shard.Shard)
	return nil
}

func (*mockClusterClient) Stop() error { return nil }

func (*mockClusterClient) BroadcastXShardTxList(_ context.Context, chainID *big.Int, hash common.Hash, deposits []*types.DepositTx) error {
	log.Info("Mock cross-shard delivery", "chain", chainID, "block", hash, "deposits", len(deposits))
	return nil
}

func (*mockClusterClient) SendShardBlockHeaderToMaster(_ context.Context, header *types.Header, amount *uint256.Int) error {
	log.Info("Mock shard header report", "block", header.Hash(), "coinbaseAmount", amount)
	return nil
}

// NewQKCBackendWithMockClient creates a backend for standalone tools and tests,
// using a mock cluster client for QKC chains and the ordinary backend otherwise.
func NewQKCBackendWithMockClient(stack *node.Node, config *ethconfig.Config) (*Ethereum, error) {
	return newBackend(stack, config, func(slavenet.ServiceConfig) (clusterClient, error) {
		return &mockClusterClient{}, nil
	})
}
