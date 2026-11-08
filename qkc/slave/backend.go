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

// Package slave assembles a standalone shard using geth's execution service.
package slave

import (
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/genesis"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/ethereum/go-ethereum/rpc"
)

// Slave shares a single execution service with its shard. Ethereum registers
// its lifecycle on Node and owns all execution resources; Shard borrows them.
// Master connections, root initialization and mining are not started here.
type Slave struct {
	Ethereum *eth.Ethereum
	Shard    *shard.Shard
}

// New prepares the local genesis, creates the execution service and registers
// native query, filter and tracing APIs. Start and close the supplied Node as
// with geth; it manages the execution service's lifecycle.
// Callers must exit the process on construction or startup failure: closing a
// Node before successful Start does not fully release execution resources.
func New(stack *node.Node, cfg *ethconfig.Config) (*Slave, error) {
	g, err := genesis.BuildShard(cfg.Genesis)
	if err != nil {
		return nil, err
	}
	executionConfig := *cfg
	executionConfig.Genesis = g.Genesis
	// Cluster synchronization does not use Ethereum snap sync or peer discovery.
	executionConfig.SyncMode = ethconfig.FullSync
	executionConfig.EthDiscoveryURLs = nil
	executionConfig.SnapDiscoveryURLs = nil
	backend, err := eth.New(stack, &executionConfig)
	if err != nil {
		return nil, err
	}
	s := &Slave{
		Ethereum: backend,
		Shard:    shard.New(backend.ChainDb(), backend.BlockChain(), backend.Miner(), nil),
	}
	// No Ethereum peer sync runs on a standalone Slave.
	backend.SetSynced()
	filterSystem := filters.NewFilterSystem(backend.APIBackend, filters.Config{
		LogCacheSize: cfg.FilterLogCacheSize, LogQueryLimit: cfg.LogQueryLimit, RangeLimit: cfg.RangeLimit,
	})
	stack.RegisterAPIs([]rpc.API{{Namespace: "eth", Service: filters.NewFilterAPI(filterSystem)}})
	stack.RegisterAPIs(tracers.APIs(backend.APIBackend))
	return s, nil
}
