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

// Package config separates QuarkChain network definitions from local settings.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"

	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/naoina/toml"
)

// NetworkConfig is shared by every node in a network. NetworkID identifies the
// P2P network, independently of the EVM ChainID in each shard's chain config.
type NetworkConfig struct {
	NetworkID uint32
	Root      RootConfig
	Shards    []params.ChainConfig
}

// RootConfig contains root-chain consensus settings. The root is not an EVM
// chain and does not inherit a shard's chain ID or PoSW configuration.
type RootConfig struct {
	PoSW *params.PoSWConfig `toml:",omitempty"`
}

// NodeConfig contains local settings for a node hosting the root and all shards.
// Eth supplies shared shard execution settings; chain definitions belong in the
// network file. NetworkFile is relative to the local TOML file when loaded.
type NodeConfig struct {
	NetworkFile string
	Node        node.Config
	Eth         ethconfig.Config
}

// TOML uses geth's case-sensitive Go field names and rejects unknown fields.
var tomlSettings = toml.Config{
	NormFieldName: func(_ reflect.Type, key string) string { return key },
	FieldToKey:    func(_ reflect.Type, field string) string { return field },
}

// DefaultNodeConfig returns geth defaults with a separate QKC data directory and
// IPC endpoint. Network definitions have no implicit Ethereum network preset.
func DefaultNodeConfig() NodeConfig {
	cfg := NodeConfig{Node: node.DefaultConfig, Eth: ethconfig.Defaults}
	cfg.Node.Name = "qkc"
	if cfg.Node.DataDir != "" {
		cfg.Node.DataDir = filepath.Join(cfg.Node.DataDir, "qkc")
	}
	cfg.Node.IPCPath = "qkc.ipc"
	cfg.Node.HTTPModules = append(slices.Clone(cfg.Node.HTTPModules), "eth")
	cfg.Node.WSModules = append(slices.Clone(cfg.Node.WSModules), "eth")
	return cfg
}

// LoadNode overlays local TOML settings on defaults. Validation is deferred so
// callers can apply explicit CLI values first. An empty file uses only defaults.
func LoadNode(file string) (*NodeConfig, error) {
	cfg := DefaultNodeConfig()
	if file != "" {
		if err := load(file, &cfg); err != nil {
			return nil, err
		}
		if cfg.NetworkFile != "" && !filepath.IsAbs(cfg.NetworkFile) {
			cfg.NetworkFile = filepath.Join(filepath.Dir(file), cfg.NetworkFile)
		}
	}
	return &cfg, nil
}

// LoadNetwork loads and validates an explicit network definition. Omitted shard
// lists default to all configured shard IDs, sorted by ID. PoSW defaults to off.
func LoadNetwork(file string) (*NetworkConfig, error) {
	var cfg NetworkConfig
	if file == "" {
		return nil, errors.New("NetworkFile is required")
	}
	if err := load(file, &cfg); err != nil {
		return nil, err
	}
	ids := make([]uint32, 0, len(cfg.Shards))
	for _, shard := range cfg.Shards {
		if shard.QuarkChain != nil {
			ids = append(ids, shard.QuarkChain.ShardID)
		}
	}
	slices.Sort(ids)
	for _, shard := range cfg.Shards {
		if shard.QuarkChain != nil && shard.QuarkChain.ShardIDs == nil {
			shard.QuarkChain.ShardIDs = slices.Clone(ids)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &cfg, nil
}

func load(file string, cfg interface{}) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tomlSettings.NewDecoder(f).Decode(cfg); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

// Validate checks network IDs, shard topology, fork order and each chain's PoSW.
func (c *NetworkConfig) Validate() error {
	if c.NetworkID == 0 {
		return errors.New("NetworkID must be positive; it is not inferred from an EVM chain ID")
	}
	if err := c.Root.PoSW.Check(); err != nil {
		return fmt.Errorf("Root: %w", err)
	}
	if len(c.Shards) == 0 {
		return errors.New("network must define at least one shard")
	}
	ids := make(map[uint32]bool, len(c.Shards))
	chainIDs := make(map[string]bool, len(c.Shards))
	for i, shard := range c.Shards {
		if shard.QuarkChain == nil {
			return fmt.Errorf("Shards[%d]: QuarkChain configuration is required", i)
		}
		id := shard.QuarkChain.ShardID
		if id == math.MaxUint32 {
			return fmt.Errorf("shard ID %d is reserved for the root chain", id)
		}
		if ids[id] {
			return fmt.Errorf("duplicate shard ID %d", id)
		}
		ids[id] = true
		if shard.ChainID == nil || shard.ChainID.Sign() <= 0 || shard.ChainID.BitLen() > 256 {
			return fmt.Errorf("shard %d: ChainID must be a positive uint256 EVM chain ID", id)
		}
		if chainIDs[shard.ChainID.String()] {
			return fmt.Errorf("duplicate EVM ChainID %s", shard.ChainID)
		}
		chainIDs[shard.ChainID.String()] = true
		if err := shard.CheckConfigForkOrder(); err != nil {
			return fmt.Errorf("shard %d: %w", id, err)
		}
	}
	if !ids[params.RootCoinbaseShardID] {
		return errors.New("network must include shard 0 for root rewards and staking")
	}
	for _, shard := range c.Shards {
		qkc := shard.QuarkChain
		seen := make(map[uint32]bool, len(qkc.ShardIDs))
		for _, id := range qkc.ShardIDs {
			if !ids[id] || seen[id] {
				return fmt.Errorf("shard %d: ShardIDs contains unknown or duplicate shard %d", qkc.ShardID, id)
			}
			seen[id] = true
		}
		if len(seen) != len(ids) {
			return fmt.Errorf("shard %d: ShardIDs must contain every network shard", qkc.ShardID)
		}
	}
	return nil
}

// Validate checks the effective local settings after CLI overrides. A zero
// Eth.NetworkId means unspecified and is resolved from the network definition,
// never from a shard's EVM chain ID. A nonzero value must match the network.
func (c *NodeConfig) Validate(network *NetworkConfig) error {
	if err := network.Validate(); err != nil {
		return err
	}
	if c.Eth.NetworkId != 0 && c.Eth.NetworkId != uint64(network.NetworkID) {
		return fmt.Errorf("Eth.NetworkId %d conflicts with NetworkID %d", c.Eth.NetworkId, network.NetworkID)
	}
	if c.Eth.Genesis != nil {
		return errors.New("Eth.Genesis is a network definition and cannot be set locally")
	}
	if c.Eth.OverrideOsaka != nil || c.Eth.OverrideAmsterdam != nil || c.Eth.OverrideBPO1 != nil || c.Eth.OverrideBPO2 != nil || c.Eth.OverrideUBT != nil {
		return errors.New("local fork overrides are not allowed; configure forks in the network file")
	}
	if c.Node.DBEngine != "" && c.Node.DBEngine != "pebble" && c.Node.DBEngine != "leveldb" {
		return fmt.Errorf("invalid Node.DBEngine %q", c.Node.DBEngine)
	}
	if c.Eth.StateScheme != "" && c.Eth.StateScheme != "hash" && c.Eth.StateScheme != "path" {
		return fmt.Errorf("invalid Eth.StateScheme %q", c.Eth.StateScheme)
	}
	if !c.Eth.SyncMode.IsValid() {
		return fmt.Errorf("invalid Eth.SyncMode %d", c.Eth.SyncMode)
	}
	for name, value := range map[string]int{
		"Node.P2P.MaxPeers": c.Node.P2P.MaxPeers, "Node.P2P.MaxPendingPeers": c.Node.P2P.MaxPendingPeers,
		"Eth.DatabaseCache": c.Eth.DatabaseCache, "Eth.TrieCleanCache": c.Eth.TrieCleanCache,
		"Eth.TrieDirtyCache": c.Eth.TrieDirtyCache, "Eth.SnapshotCache": c.Eth.SnapshotCache,
	} {
		if value < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	for name, port := range map[string]int{"Node.HTTPPort": c.Node.HTTPPort, "Node.WSPort": c.Node.WSPort, "Node.AuthPort": c.Node.AuthPort} {
		if port < 0 || port > math.MaxUint16 {
			return fmt.Errorf("%s must be between 0 and 65535", name)
		}
	}
	for name, addr := range map[string]string{"Node.P2P.ListenAddr": c.Node.P2P.ListenAddr, "Node.P2P.DiscAddr": c.Node.P2P.DiscAddr} {
		if addr == "" {
			continue
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("%s: invalid port %q", name, port)
		}
	}
	c.Eth.NetworkId = uint64(network.NetworkID)
	return nil
}
