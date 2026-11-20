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

package config

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
)

// RootConfig contains the root node settings and the referenced shard inputs.
// Slaves and ShardGeneses follow the consensus ChainIDs order.
type RootConfig struct {
	Node         node.Config
	NetworkID    uint64
	Genesis      *RootGenesis
	ShardGeneses []*core.Genesis
	Slaves       []clusternet.SlaveInfo
}

// RootGenesis holds the fixed R0 inputs and the effective root consensus rules.
type RootGenesis struct {
	Timestamp  uint64
	Difficulty *big.Int
	Coinbase   common.Address
	Config     *params.QuarkChainConfig
}

// Shared rules come exclusively from the shard TOMLs. Keeping the on-disk
// consensus table narrow also catches an accidental duplicate definition.
type rootGenesisTOML struct {
	Timestamp  *uint64
	Difficulty *big.Int
	Coinbase   common.Address
	Config     struct {
		ConsensusType params.ConsensusType
		PoSW          *params.PoSWConfig
	}
}

func (g *rootGenesisTOML) resolve(shared *params.QuarkChainConfig) (*RootGenesis, error) {
	if g.Timestamp == nil {
		return nil, errors.New("Timestamp is required")
	}
	if g.Difficulty == nil || g.Difficulty.Sign() <= 0 || g.Difficulty.BitLen() > 256 {
		return nil, errors.New("Difficulty must be a positive uint256")
	}
	cfg := *shared
	cfg.ConsensusType, cfg.PoSW = g.Config.ConsensusType, g.Config.PoSW
	if err := cfg.ConsensusType.Check(); err != nil {
		return nil, err
	}
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	if g.Difficulty.Cmp(cfg.PoSW.MinimumDifficulty) < 0 {
		return nil, errors.New("Difficulty is below MinimumDifficulty")
	}
	return &RootGenesis{Timestamp: *g.Timestamp, Difficulty: g.Difficulty, Coinbase: g.Coinbase, Config: &cfg}, nil
}

// LoadRoot loads a root TOML and resolves its shard TOML references relative
// to the file. It neither opens databases nor connects to shards. An omitted
// Root.Genesis is left nil so the caller can reopen an initialized root DB.
func LoadRoot(file string) (*RootConfig, error) {
	var spec struct {
		Node node.Config
		Root struct {
			NetworkID    uint64
			ShardConfigs []string
			Genesis      *rootGenesisTOML
		}
	}
	master := DefaultNodeConfig(nil)
	spec.Node = master.Node
	if err := load(file, &spec); err != nil {
		return nil, err
	}
	master.Node, master.PrivateHost, master.PrivatePort = spec.Node, spec.Node.PrivateHost, spec.Node.PrivatePort
	if err := master.Validate(spec.Root.NetworkID); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(spec.Root.ShardConfigs) == 0 {
		return nil, fmt.Errorf("%s: Root.ShardConfigs must list at least one shard", file)
	}
	cfg := &RootConfig{Node: spec.Node, NetworkID: spec.Root.NetworkID}
	var shared *params.QuarkChainConfig
	indices := make(map[string]int)
	endpoints := make(map[string]bool)
	for _, ref := range spec.Root.ShardConfigs {
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(file), path)
		}
		g, slave, err := loadRootShard(path, cfg.NetworkID)
		if err != nil {
			return nil, fmt.Errorf("%s: shard %q: %w", file, ref, err)
		}
		q := g.Config.QuarkChain
		if shared == nil {
			shared = q
			cfg.ShardGeneses = make([]*core.Genesis, len(q.ChainIDs))
			cfg.Slaves = make([]clusternet.SlaveInfo, len(q.ChainIDs))
			for i, id := range q.ChainIDs {
				indices[id.String()] = i
			}
		} else if !shared.EqualNetworkRules(q) {
			return nil, fmt.Errorf("%s: ChainIDs, RewardTaxRate and MaxBlocksPerRootBlock must agree across shards", path)
		}
		i := indices[slave.ChainID.String()] // ValidateQuarkChain checks local membership.
		if cfg.ShardGeneses[i] != nil {
			return nil, fmt.Errorf("%s: duplicate shard chain ID %v", path, slave.ChainID)
		}
		addr := net.JoinHostPort(canonicalHost(slave.Host), strconv.Itoa(int(slave.Port)))
		if endpoints[addr] {
			return nil, fmt.Errorf("%s: duplicate shard private endpoint %s", path, addr)
		}
		endpoints[addr] = true
		cfg.ShardGeneses[i], cfg.Slaves[i] = g, slave
	}
	for i, g := range cfg.ShardGeneses {
		if g == nil {
			return nil, fmt.Errorf("%s: missing shard config for chain ID %v", file, shared.ChainIDs[i])
		}
	}
	if spec.Root.Genesis != nil {
		genesis, err := spec.Root.Genesis.resolve(shared)
		if err != nil {
			return nil, fmt.Errorf("%s: Root.Genesis: %w", file, err)
		}
		cfg.Genesis = genesis
	}
	return cfg, nil
}

func loadRootShard(path string, networkID uint64) (*core.Genesis, clusternet.SlaveInfo, error) {
	// Decode native geth TOML, including unrelated runtime sections so a shard
	// can use the same file for standalone startup.
	var spec struct {
		Eth      ethconfig.Config
		Node     node.Config
		Metrics  metrics.Config
		Ethstats struct{ URL string }
	}
	var slave clusternet.SlaveInfo
	if err := load(path, &spec); err != nil {
		return nil, slave, err
	}
	if err := checkForkOverrides(&spec.Eth); err != nil {
		return nil, slave, err
	}
	g := spec.Eth.Genesis
	if g == nil || g.Config == nil || g.Config.QuarkChain == nil {
		return nil, slave, errors.New("Eth.Genesis.Config.QuarkChain is required")
	}
	if err := g.ValidateQuarkChain(); err != nil {
		return nil, slave, err
	}
	if err := g.Config.CheckConfigForkOrder(); err != nil {
		return nil, slave, err
	}
	// As in eth.New, an unset/zero NetworkId selects the EVM chain ID.
	id := spec.Eth.NetworkId
	if id == 0 {
		id = g.Config.ChainID.Uint64()
	}
	if id != networkID {
		return nil, slave, fmt.Errorf("shard network ID %d differs from root NetworkID %d", id, networkID)
	}
	if err := checkPrivateEndpoint(spec.Node.PrivateHost, spec.Node.PrivatePort); err != nil {
		return nil, slave, err
	}
	return g, clusternet.SlaveInfo{Host: spec.Node.PrivateHost, Port: uint16(spec.Node.PrivatePort), ChainID: g.Config.ChainID}, nil
}
