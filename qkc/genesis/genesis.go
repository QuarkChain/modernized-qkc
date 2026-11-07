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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
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

// Load reads the GenesisFile paths of a loaded cluster and constructs its full
// network genesis. Config.LoadCluster resolves these paths relative to the
// deployment file. Runtime addresses, directories and coinbases do not affect
// genesis. The inputs are not modified and all state computation is in memory.
func Load(cluster *config.ClusterConfig) (*Bundle, error) {
	if cluster == nil {
		return nil, errors.New("cluster configuration is required")
	}
	if cluster.Master.GenesisFile == "" {
		return nil, errors.New("Master.GenesisFile is required")
	}
	seed, err := readRoot(cluster.Master.GenesisFile)
	if err != nil {
		return nil, err
	}
	// Fresh containers keep loaded genesis definitions and all derived fields
	// independent of caller-owned execution settings or previously loaded files.
	loaded := &config.ClusterConfig{
		Master: config.NodeConfig{
			Role: cluster.Master.Role, ChainID: cluster.Master.ChainID,
			GenesisFile: cluster.Master.GenesisFile, Root: seed.Config,
		},
		Slaves: make([]config.NodeConfig, len(cluster.Slaves)),
	}
	for i, slave := range cluster.Slaves {
		loaded.Slaves[i] = config.NodeConfig{
			Role: slave.Role, ChainID: slave.ChainID, GenesisFile: slave.GenesisFile,
			Eth: new(ethconfig.Config),
		}
		if err := loaded.Slaves[i].LoadGenesis(); err != nil {
			return nil, fmt.Errorf("Slaves[%d]: %w", i, err)
		}
	}
	if err := loaded.ValidateGenesis(); err != nil {
		return nil, err
	}
	shards := make(map[string]*config.NodeConfig, len(loaded.Slaves))
	for i := range loaded.Slaves {
		slave := &loaded.Slaves[i]
		shards[slave.ChainID.String()] = slave
	}
	order := loaded.Slaves[0].Eth.Genesis.Config.QuarkChain.ChainIDs
	bundle := &Bundle{RootConfig: seed.Config, Shards: make([]Shard, len(order))}
	headers := make([]*types.Header, len(order))
	for i, id := range order {
		slave := shards[id.String()]
		shard, err := buildShard(slave.Eth.Genesis)
		if err != nil {
			return nil, fmt.Errorf("chain %v (%s): %w", id, slave.GenesisFile, err)
		}
		bundle.Shards[i] = *shard
		headers[i] = shard.Block.Header()
	}
	bundle.Root, err = buildRoot(cluster.Master.GenesisFile, seed, headers)
	if err != nil {
		return nil, err
	}
	return bundle, nil
}

// decodeJSON requires exactly one JSON document. Native types retain their
// existing JSON codecs; plain input structs additionally reject unknown fields.
func decodeJSON(file string, target any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s: expected a single JSON document", file)
	}
	return nil
}
