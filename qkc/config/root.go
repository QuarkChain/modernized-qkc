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

// Package config loads QuarkChain root and shard TOML settings.
package config

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/root"
	"github.com/naoina/toml"
)

const rootNodeName = "qkc-master"

// TOML uses geth's case-sensitive Go field names and rejects unknown fields.
var tomlSettings = toml.Config{
	NormFieldName: func(_ reflect.Type, key string) string { return key },
	FieldToKey:    func(_ reflect.Type, field string) string { return field },
}

// RootConfig contains the root node settings and the referenced shard inputs.
// Shards follow the consensus ChainIDs order.
type RootConfig struct {
	Node      node.Config
	NetworkID uint64
	Genesis   *root.RootGenesis
	Shards    []ShardConfig
}

// ShardConfig is one shard's TOML path, genesis and private endpoint.
type ShardConfig struct {
	Path    string
	Genesis *core.Genesis
	Slave   clusternet.SlaveInfo
}

// ShardGeneses returns the shard genesis specifications in ChainIDs order.
func (c *RootConfig) ShardGeneses() []*core.Genesis {
	genesis := make([]*core.Genesis, len(c.Shards))
	for i, shard := range c.Shards {
		genesis[i] = shard.Genesis
	}
	return genesis
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

func (g *rootGenesisTOML) resolve(shared *params.QuarkChainConfig) (*root.RootGenesis, error) {
	if g.Timestamp == nil {
		return nil, errors.New("Timestamp is required")
	}
	cfg := *shared
	cfg.ConsensusType, cfg.PoSW = g.Config.ConsensusType, g.Config.PoSW
	genesis := &root.RootGenesis{Timestamp: *g.Timestamp, Difficulty: g.Difficulty, Coinbase: g.Coinbase, Config: &cfg}
	if err := genesis.Validate(); err != nil {
		return nil, err
	}
	return genesis, nil
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
	spec.Node = node.DefaultConfig
	spec.Node.Name = rootNodeName
	spec.Node.IPCPath = rootNodeName + ".ipc"
	if spec.Node.DataDir != "" {
		spec.Node.DataDir = filepath.Join(spec.Node.DataDir, "qkc", "master")
	}
	if err := load(file, &spec); err != nil {
		return nil, err
	}
	if spec.Root.NetworkID == 0 {
		return nil, fmt.Errorf("%s: Root.NetworkID must be positive", file)
	}
	if len(spec.Root.ShardConfigs) == 0 {
		return nil, fmt.Errorf("%s: Root.ShardConfigs must list at least one shard", file)
	}
	cfg := &RootConfig{Node: spec.Node, NetworkID: spec.Root.NetworkID}
	var shared *params.QuarkChainConfig
	indices := make(map[string]int)
	endpoints := make(map[string]string)
	if host, port := spec.Node.PrivateHost, spec.Node.PrivatePort; host != "" || port != 0 {
		// The root uses this endpoint as a bind address.
		if err := checkPrivateEndpoint(host, port, true); err != nil {
			return nil, fmt.Errorf("%s: root private endpoint: %w", file, err)
		}
		endpoints[net.JoinHostPort(canonicalHost(host), strconv.Itoa(port))] = file
	}
	for _, ref := range spec.Root.ShardConfigs {
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(file), path)
		}
		shardNode, shardEth, err := loadShard(path, cfg.NetworkID, shared)
		if err != nil {
			return nil, fmt.Errorf("%s: shard %q: %w", file, ref, err)
		}
		slave := clusternet.SlaveInfo{Host: shardNode.PrivateHost, Port: uint16(shardNode.PrivatePort), ChainID: shardEth.Genesis.Config.ChainID}
		q := shardEth.Genesis.Config.QuarkChain
		if shared == nil {
			shared = q
			cfg.Shards = make([]ShardConfig, len(q.ChainIDs))
			for i, id := range q.ChainIDs {
				indices[id.String()] = i
			}
		}
		i := indices[slave.ChainID.String()] // ValidateQuarkChain checks local membership.
		if cfg.Shards[i].Genesis != nil {
			return nil, fmt.Errorf("%s: duplicate shard chain ID %v", path, slave.ChainID)
		}
		addr := net.JoinHostPort(canonicalHost(slave.Host), strconv.Itoa(int(slave.Port)))
		if owner, ok := endpoints[addr]; ok {
			return nil, fmt.Errorf("%s: duplicate private endpoint %s also configured in %s", path, addr, owner)
		}
		endpoints[addr] = path
		cfg.Shards[i] = ShardConfig{Path: path, Genesis: shardEth.Genesis, Slave: slave}
	}
	for i, shard := range cfg.Shards {
		if shard.Genesis == nil {
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

// LoadShard loads the node and Ethereum settings for a standalone shard.
func LoadShard(path string) (node.Config, ethconfig.Config, error) {
	return loadShard(path, 0, nil)
}

// A zero networkID skips comparison with a root deployment.
func loadShard(path string, networkID uint64, shared *params.QuarkChainConfig) (node.Config, ethconfig.Config, error) {
	// Decode native geth TOML, including unrelated runtime sections so a shard
	// can use the same file for standalone startup.
	var spec struct {
		Eth      ethconfig.Config
		Node     node.Config
		Metrics  metrics.Config
		Ethstats struct{ URL string }
	}
	spec.Eth = ethconfig.Defaults
	spec.Node = node.DefaultConfig
	spec.Node.Name = "geth"
	spec.Node.IPCPath = "geth.ipc"
	spec.Node.HTTPModules = append(spec.Node.HTTPModules, "eth")
	spec.Node.WSModules = append(spec.Node.WSModules, "eth")
	if err := load(path, &spec); err != nil {
		return node.Config{}, ethconfig.Config{}, err
	}
	if spec.Eth.OverrideOsaka != nil || spec.Eth.OverrideAmsterdam != nil || spec.Eth.OverrideBPO1 != nil || spec.Eth.OverrideBPO2 != nil || spec.Eth.OverrideUBT != nil {
		return node.Config{}, ethconfig.Config{}, errors.New("local fork overrides are not allowed; configure forks in Genesis")
	}
	g := spec.Eth.Genesis
	if err := root.ValidateShardGenesis(g, shared); err != nil {
		return node.Config{}, ethconfig.Config{}, err
	}
	// As in eth.New, an unset/zero NetworkId selects the EVM chain ID.
	id := spec.Eth.NetworkId
	if id == 0 {
		id = g.Config.ChainID.Uint64()
	}
	if networkID != 0 && id != networkID {
		return node.Config{}, ethconfig.Config{}, fmt.Errorf("shard network ID %d differs from root NetworkID %d", id, networkID)
	}
	if err := checkPrivateEndpoint(spec.Node.PrivateHost, spec.Node.PrivatePort, false); err != nil {
		return node.Config{}, ethconfig.Config{}, err
	}
	return spec.Node, spec.Eth, nil
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

// canonicalHost normalizes IP literals and hostname case without resolving DNS.
func canonicalHost(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(host)
}

func checkPrivateEndpoint(host string, port int, allowWildcard bool) error {
	ip, err := netip.ParseAddr(host)
	if host == "" || strings.ContainsAny(host, " \t\r\n/[]") ||
		(strings.Contains(host, ":") && err != nil) {
		return fmt.Errorf("PrivateHost must be a host without brackets or a port: %q", host)
	}
	if !allowWildcard && err == nil && ip.Unmap().IsUnspecified() {
		return fmt.Errorf("PrivateHost must be a concrete host: %q", host)
	}
	if port < 1 || port > math.MaxUint16 {
		return errors.New("PrivatePort must be between 1 and 65535")
	}
	return nil
}
