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

// Package genesis constructs a QKC network's genesis blocks without opening
// deployment databases or starting services.
package genesis

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// Bundle contains the validated genesis definitions and their blocks. RootConfig
// and Shard.Genesis.Config retain the full consensus settings: genesis hashes
// alone do not commit to every PoSW setting. Shards follow the common ChainIDs
// order, with the root reward chain first.
type Bundle struct {
	RootConfig *config.RootConfig
	Root       *qkctypes.RootBlock
	Shards     []Shard
}

// Shard pairs a complete native genesis specification with its derived block.
// The specification can later be passed to core.SetupGenesisBlock.
type Shard struct {
	Genesis *core.Genesis
	Block   *types.Block
}

// Build constructs network genesis from definitions prepared by ClusterConfig.LoadGenesis.
// It does not read files or change the definitions. The returned bundle retains
// those definitions; callers must not mutate them while the bundle is in use.
func Build(cluster *config.ClusterConfig) (*Bundle, error) {
	if cluster == nil || cluster.Master.Root == nil || len(cluster.Slaves) == 0 {
		return nil, errors.New("loaded network genesis is required")
	}
	shards := make(map[string]*core.Genesis, len(cluster.Slaves))
	for i, slave := range cluster.Slaves {
		if slave.Eth == nil || slave.Eth.Genesis == nil || slave.Eth.Genesis.Config == nil || slave.ChainID == nil {
			return nil, fmt.Errorf("Slaves[%d]: loaded shard genesis is required", i)
		}
		shards[slave.ChainID.String()] = slave.Eth.Genesis
	}
	first := cluster.Slaves[0].Eth.Genesis.Config.QuarkChain
	if first == nil {
		return nil, errors.New("loaded QuarkChain configuration is required")
	}
	bundle := &Bundle{RootConfig: cluster.Master.Root.RootConfig, Shards: make([]Shard, len(first.ChainIDs))}
	headers := make([]*types.Header, len(first.ChainIDs))
	for i, id := range first.ChainIDs {
		spec := shards[id.String()]
		if spec == nil {
			return nil, fmt.Errorf("chain %v: loaded shard genesis is required", id)
		}
		shard, err := buildShard(spec)
		if err != nil {
			return nil, fmt.Errorf("chain %v: %w", id, err)
		}
		bundle.Shards[i] = *shard
		headers[i] = shard.Block.Header()
	}
	root, err := buildRoot(cluster.Master.Root, headers)
	if err != nil {
		return nil, err
	}
	bundle.Root = root
	return bundle, nil
}
