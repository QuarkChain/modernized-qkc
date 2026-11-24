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
	"runtime"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
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
	Genesis   *RootGenesis
	Shards    []ShardConfig
}

// ShardConfig is one shard's genesis and its private endpoint.
type ShardConfig struct {
	Genesis *core.Genesis
	Slave   clusternet.SlaveInfo
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
	cfg := *shared
	cfg.ConsensusType, cfg.PoSW = g.Config.ConsensusType, g.Config.PoSW
	genesis := &RootGenesis{Timestamp: *g.Timestamp, Difficulty: g.Difficulty, Coinbase: g.Coinbase, Config: &cfg}
	if err := genesis.Validate(); err != nil {
		return nil, err
	}
	return genesis, nil
}

// Validate checks the root consensus rules and the fixed R0 difficulty.
func (g *RootGenesis) Validate() error {
	if g == nil || g.Config == nil {
		return errors.New("missing root genesis")
	}
	if g.Difficulty == nil || g.Difficulty.Sign() <= 0 || g.Difficulty.BitLen() > 256 {
		return errors.New("Difficulty must be a positive uint256")
	}
	if err := g.Config.ConsensusType.Check(); err != nil {
		return err
	}
	if err := g.Config.Check(); err != nil {
		return err
	}
	if g.Difficulty.Cmp(g.Config.PoSW.MinimumDifficulty) < 0 {
		return errors.New("Difficulty is below MinimumDifficulty")
	}
	return nil
}

// Validate checks the root genesis and that every shard genesis input matches
// its chain ID position and the root's network rules. LoadRoot already checks
// each shard while loading; this also covers a RootConfig built by hand.
func (c *RootConfig) Validate() error {
	if c == nil || c.Genesis == nil {
		return errors.New("missing root genesis")
	}
	if err := c.Genesis.Validate(); err != nil {
		return err
	}
	rules := c.Genesis.Config
	if len(c.Shards) != len(rules.ChainIDs) {
		return errors.New("shard genesis count differs from configured chain IDs")
	}
	for i, sc := range c.Shards {
		shard, id := sc.Genesis, rules.ChainIDs[i]
		if shard == nil || shard.Config == nil || shard.Config.ChainID == nil || shard.Config.ChainID.Cmp(id) != 0 || shard.Config.QuarkChain == nil {
			return fmt.Errorf("missing shard genesis for chain %s", id)
		}
		if !shard.Config.QuarkChain.EqualNetworkRules(rules) {
			return fmt.Errorf("shard %s network rules differ from root", id)
		}
		if err := checkShardGenesis(shard); err != nil {
			return fmt.Errorf("shard %s genesis: %w", id, err)
		}
	}
	return nil
}

func checkShardGenesis(g *core.Genesis) error {
	if err := g.ValidateQuarkChain(); err != nil {
		return err
	}
	return g.Config.CheckConfigForkOrder()
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
	if err := checkRootNode(&spec.Node); err != nil {
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
		shard, err := loadRootShard(path, cfg.NetworkID)
		if err != nil {
			return nil, fmt.Errorf("%s: shard %q: %w", file, ref, err)
		}
		slave := shard.Slave
		q := shard.Genesis.Config.QuarkChain
		if shared == nil {
			shared = q
			cfg.Shards = make([]ShardConfig, len(q.ChainIDs))
			for i, id := range q.ChainIDs {
				indices[id.String()] = i
			}
		} else if !shared.EqualNetworkRules(q) {
			return nil, fmt.Errorf("%s: ChainIDs, RewardTaxRate and MaxBlocksPerRootBlock must agree across shards", path)
		}
		i := indices[slave.ChainID.String()] // ValidateQuarkChain checks local membership.
		if cfg.Shards[i].Genesis != nil {
			return nil, fmt.Errorf("%s: duplicate shard chain ID %v", path, slave.ChainID)
		}
		addr := net.JoinHostPort(canonicalHost(slave.Host), strconv.Itoa(int(slave.Port)))
		if endpoints[addr] {
			return nil, fmt.Errorf("%s: duplicate shard private endpoint %s", path, addr)
		}
		endpoints[addr] = true
		cfg.Shards[i] = shard
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

func loadRootShard(path string, networkID uint64) (ShardConfig, error) {
	// Decode native geth TOML, including unrelated runtime sections so a shard
	// can use the same file for standalone startup.
	var spec struct {
		Eth      ethconfig.Config
		Node     node.Config
		Metrics  metrics.Config
		Ethstats struct{ URL string }
	}
	if err := load(path, &spec); err != nil {
		return ShardConfig{}, err
	}
	if spec.Eth.OverrideOsaka != nil || spec.Eth.OverrideAmsterdam != nil || spec.Eth.OverrideBPO1 != nil || spec.Eth.OverrideBPO2 != nil || spec.Eth.OverrideUBT != nil {
		return ShardConfig{}, errors.New("local fork overrides are not allowed; configure forks in Genesis")
	}
	g := spec.Eth.Genesis
	if g == nil || g.Config == nil || g.Config.QuarkChain == nil {
		return ShardConfig{}, errors.New("Eth.Genesis.Config.QuarkChain is required")
	}
	if err := checkShardGenesis(g); err != nil {
		return ShardConfig{}, err
	}
	// As in eth.New, an unset/zero NetworkId selects the EVM chain ID.
	id := spec.Eth.NetworkId
	if id == 0 {
		id = g.Config.ChainID.Uint64()
	}
	if id != networkID {
		return ShardConfig{}, fmt.Errorf("shard network ID %d differs from root NetworkID %d", id, networkID)
	}
	if err := checkPrivateEndpoint(spec.Node.PrivateHost, spec.Node.PrivatePort); err != nil {
		return ShardConfig{}, err
	}
	slave := clusternet.SlaveInfo{Host: spec.Node.PrivateHost, Port: uint16(spec.Node.PrivatePort), ChainID: g.Config.ChainID}
	return ShardConfig{Genesis: g, Slave: slave}, nil
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

func checkRootNode(cfg *node.Config) error {
	if cfg.DBEngine != "" && cfg.DBEngine != "pebble" && cfg.DBEngine != "leveldb" {
		return fmt.Errorf("invalid Node.DBEngine %q", cfg.DBEngine)
	}
	for name, value := range map[string]int{
		"Node.P2P.MaxPeers": cfg.P2P.MaxPeers, "Node.P2P.MaxPendingPeers": cfg.P2P.MaxPendingPeers,
	} {
		if value < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	for name, port := range map[string]int{
		"Node.HTTPPort":    cfg.HTTPPort,
		"Node.WSPort":      cfg.WSPort,
		"Node.AuthPort":    cfg.AuthPort,
		"Node.PrivatePort": cfg.PrivatePort,
	} {
		if port < 0 || port > math.MaxUint16 {
			return fmt.Errorf("%s must be between 0 and 65535", name)
		}
	}
	for name, addr := range map[string]string{"Node.P2P.ListenAddr": cfg.P2P.ListenAddr, "Node.P2P.DiscAddr": cfg.P2P.DiscAddr} {
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
	// macOS has a 104-byte Unix socket path, including the terminator.
	if endpoint := cfg.IPCEndpoint(); runtime.GOOS != "windows" && len(endpoint) > 103 {
		return fmt.Errorf("IPC endpoint exceeds Unix socket path limit: %s", endpoint)
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

func checkPrivateEndpoint(host string, port int) error {
	ip, err := netip.ParseAddr(host)
	if host == "" || strings.ContainsAny(host, " \t\r\n/[]") ||
		(strings.Contains(host, ":") && err != nil) || (err == nil && ip.Unmap().IsUnspecified()) {
		return fmt.Errorf("PrivateHost must be a concrete host without brackets or a port: %q", host)
	}
	if port < 1 || port > math.MaxUint16 {
		return errors.New("PrivatePort must be between 1 and 65535")
	}
	return nil
}
