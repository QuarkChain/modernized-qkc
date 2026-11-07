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

// Package config loads QuarkChain deployments and their genesis configurations.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"slices"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/naoina/toml"
)

// RootConfig contains the consensus settings in the root genesis's config field.
// The root is not an EVM chain and does not inherit a shard's configuration.
type RootConfig struct {
	ConsensusType params.ConsensusType `json:"consensusType"`
	PoSW          *params.PoSWConfig   `json:"posw"`
}

const (
	RoleMaster = "master"
	RoleSlave  = "slave"
)

// ClusterConfig describes a deployment. NetworkID identifies the P2P network,
// independently of the EVM chain IDs in the shard genesis configurations.
type ClusterConfig struct {
	NetworkID uint64
	Master    NodeConfig
	Slaves    []NodeConfig
}

// NodeConfig contains the effective settings of one process. Master has no
// ChainID, PrivateHost, PrivatePort or Eth settings. A slave's PrivateHost and
// PrivatePort define its internal TCP endpoint, used both for listening and for
// dialing by Master and the other slaves. The TOML PublicHost and PublicPort
// keys configure Node.HTTPHost and Node.HTTPPort without duplicating their state.
type NodeConfig struct {
	ChainID         *big.Int
	GenesisFile     string // Relative to the deployment file; shared rules live in genesis.
	PrivateHost     string
	PrivatePort     int
	CoinbaseAddress common.Address // Local mining reward recipient; not a network rule.
	Node            node.Config
	Eth             *ethconfig.Config

	Role           string            `toml:"-"`
	Root           *RootConfig       `toml:"-"` // Loaded from Master's genesis; slaves use Eth.Genesis.
	ShardEndpoints map[string]string `toml:"-"` // Keyed by decimal chain ID; derived for Master.
}

// TOML uses geth's case-sensitive Go field names and rejects unknown fields.
var tomlSettings = toml.Config{
	NormFieldName: func(_ reflect.Type, key string) string { return key },
	FieldToKey:    func(_ reflect.Type, field string) string { return field },
}

// DefaultNodeConfig returns per-process geth defaults. A nil chainID selects
// Master; a non-nil ID selects the slave for that EVM chain ID.
func DefaultNodeConfig(chainID *big.Int) NodeConfig {
	cfg := NodeConfig{Role: RoleMaster, Node: node.DefaultConfig}
	name := "master"
	pathName := name
	if chainID != nil {
		cfg.Role, cfg.ChainID = RoleSlave, new(big.Int).Set(chainID)
		name = fmt.Sprintf("shard-%v", chainID)
		pathName = name
		if chainID.BitLen() > 64 && runtime.GOOS != "windows" {
			// Keep the full ID in Node.Name, but bound Unix path components.
			hash := sha256.Sum256(chainID.Bytes())
			pathName = "shard-" + base64.RawURLEncoding.EncodeToString(hash[:12])
		}
		eth := ethconfig.Defaults
		// TOML text decoding mutates big integers in place. Keep defaults and
		// other shard entries independent when overriding these values.
		eth.Miner.GasPrice = new(big.Int).Set(eth.Miner.GasPrice)
		eth.GPO.MaxPrice = new(big.Int).Set(eth.GPO.MaxPrice)
		eth.GPO.IgnorePrice = new(big.Int).Set(eth.GPO.IgnorePrice)
		cfg.Eth = &eth
		cfg.Node.P2P.ListenAddr, cfg.Node.P2P.DiscAddr = "", ""
		cfg.Node.P2P.MaxPeers = 0
		cfg.Node.P2P.NoDiscovery, cfg.Node.P2P.NoDial = true, true
		cfg.Node.P2P.DiscoveryV4, cfg.Node.P2P.DiscoveryV5 = false, false
		cfg.Node.HTTPModules = append(slices.Clone(cfg.Node.HTTPModules), "eth")
		cfg.Node.WSModules = append(slices.Clone(cfg.Node.WSModules), "eth")
	}
	cfg.Node.Name = "qkc-" + name
	if cfg.Node.DataDir != "" {
		cfg.Node.DataDir = filepath.Join(cfg.Node.DataDir, "qkc", pathName)
	}
	cfg.Node.IPCPath = "qkc-" + pathName + ".ipc"
	return cfg
}

// UnmarshalTOML reads identity first, then overlays explicit settings on that
// process's defaults. Both passes use the same strict schema; zero and false
// values remain explicit overrides, including in array-of-table entries.
func (c *NodeConfig) UnmarshalTOML(decode func(interface{}) error) error {
	// Decode execution fields directly: ethconfig.Config's generated decoder
	// replaces nested tables instead of overlaying their individual fields.
	type executionConfig ethconfig.Config
	type plain struct {
		ChainID         *big.Int
		GenesisFile     string
		PrivateHost     string
		PrivatePort     int
		PublicHost      *string
		PublicPort      *int
		CoinbaseAddress common.Address
		Node            node.Config
		Eth             *executionConfig
	}
	var identity plain
	if err := decode(&identity); err != nil {
		return err
	}
	cfg := DefaultNodeConfig(identity.ChainID)
	overlay := plain{
		ChainID: cfg.ChainID, PrivateHost: cfg.PrivateHost, PrivatePort: cfg.PrivatePort,
		CoinbaseAddress: cfg.CoinbaseAddress, Node: cfg.Node, Eth: (*executionConfig)(cfg.Eth),
	}
	if err := decode(&overlay); err != nil {
		return err
	}
	if overlay.Eth != nil && overlay.Eth.Genesis != nil {
		return errors.New("Eth.Genesis must be loaded from GenesisFile, not configured in deployment TOML")
	}
	// Public keys take precedence over the underlying geth HTTP settings. Pointers
	// preserve explicit empty hosts and zero ports as overrides.
	if overlay.PublicHost != nil {
		overlay.Node.HTTPHost = *overlay.PublicHost
	}
	if overlay.PublicPort != nil {
		overlay.Node.HTTPPort = *overlay.PublicPort
	}
	cfg.ChainID, cfg.PrivateHost, cfg.PrivatePort = overlay.ChainID, overlay.PrivateHost, overlay.PrivatePort
	cfg.GenesisFile = overlay.GenesisFile
	cfg.Node, cfg.Eth = overlay.Node, (*ethconfig.Config)(overlay.Eth)
	cfg.CoinbaseAddress = overlay.CoinbaseAddress
	*c = cfg
	return nil
}

// LoadCluster decodes a deployment and resolves GenesisFile paths relative to it.
// It does not read genesis files or validate the effective configuration. Callers
// select which processes to load and validate after applying explicit overrides.
func LoadCluster(file string) (*ClusterConfig, error) {
	if file == "" {
		return nil, errors.New("deployment file is required")
	}
	cfg := ClusterConfig{Master: DefaultNodeConfig(nil)}
	if err := load(file, &cfg); err != nil {
		return nil, err
	}
	if path := cfg.Master.GenesisFile; path != "" && !filepath.IsAbs(path) {
		cfg.Master.GenesisFile = filepath.Join(filepath.Dir(file), path)
	}
	for i := range cfg.Slaves {
		if path := cfg.Slaves[i].GenesisFile; path != "" && !filepath.IsAbs(path) {
			cfg.Slaves[i].GenesisFile = filepath.Join(filepath.Dir(file), path)
		}
	}
	return &cfg, nil
}

// LoadGenesis reads genesis files for every process, for whole-deployment checks
// such as checkconfig. Loading one process uses NodeConfig.LoadGenesis instead.
func (c *ClusterConfig) LoadGenesis() error {
	if err := c.Master.LoadGenesis(); err != nil {
		return fmt.Errorf("Master: %w", err)
	}
	for i := range c.Slaves {
		if err := c.Slaves[i].LoadGenesis(); err != nil {
			return fmt.Errorf("Slaves[%d]: %w", i, err)
		}
	}
	return nil
}

// LoadGenesis reads only this process's GenesisFile, as resolved by LoadCluster.
// Shards retain the native genesis in Eth.Genesis; Master retains its consensus
// config in Root. This does not validate configurations or initialize a database.
func (c *NodeConfig) LoadGenesis() error {
	if c.GenesisFile == "" {
		return errors.New("GenesisFile is required")
	}
	if c.Role != RoleMaster && c.Role != RoleSlave {
		return fmt.Errorf("invalid process role %q", c.Role)
	}
	if c.Role == RoleSlave && c.Eth == nil {
		return errors.New("slave requires execution settings")
	}
	data, err := os.ReadFile(c.GenesisFile)
	if err != nil {
		return err
	}
	if c.Role == RoleMaster {
		var genesis struct {
			Config *RootConfig `json:"config"`
		}
		if err := json.Unmarshal(data, &genesis); err != nil {
			return fmt.Errorf("%s: %w", c.GenesisFile, err)
		}
		c.Root = genesis.Config
	} else {
		var genesis core.Genesis
		if err := json.Unmarshal(data, &genesis); err != nil {
			return fmt.Errorf("%s: %w", c.GenesisFile, err)
		}
		c.Eth.Genesis = &genesis
	}
	return nil
}

// SelectNode selects one process. Only Master's effective configuration includes
// the shard endpoint map, derived from deployment entries without duplication.
func (c *ClusterConfig) SelectNode(role string, chainID *big.Int) (*NodeConfig, error) {
	var selected *NodeConfig
	switch role {
	case RoleMaster:
		if chainID != nil {
			return nil, errors.New("master cannot select a chain ID")
		}
		selected = &c.Master
		selected.ShardEndpoints = make(map[string]string, len(c.Slaves))
		for _, shard := range c.Slaves {
			if shard.ChainID != nil {
				selected.ShardEndpoints[shard.ChainID.String()] = shard.PrivateEndpoint()
			}
		}
	case RoleSlave:
		if chainID == nil {
			return nil, errors.New("slave process requires a chain ID")
		}
		for i := range c.Slaves {
			if c.Slaves[i].ChainID != nil && c.Slaves[i].ChainID.Cmp(chainID) == 0 {
				selected = &c.Slaves[i]
				break
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("chain ID %v is not in the deployment", chainID)
		}
	default:
		return nil, errors.New("set --qkc.role to master or slave")
	}
	return selected, nil
}

// PrivateEndpoint returns the shard's internal TCP address. An empty host means
// no cluster listener, as for Master. IPv6 hosts are joined with brackets.
func (c *NodeConfig) PrivateEndpoint() string {
	if c.PrivateHost == "" {
		return ""
	}
	return net.JoinHostPort(c.PrivateHost, strconv.Itoa(c.PrivatePort))
}

// Validate checks the complete deployment after genesis loading and overrides.
// It does not load files. Genesis rules and process settings are checked separately.
func (c *ClusterConfig) Validate() error {
	if c.NetworkID == 0 {
		return errors.New("NetworkID must be positive; it is not inferred from an EVM chain ID")
	}
	if err := c.ValidateGenesis(); err != nil {
		return err
	}
	if err := c.Master.validateRuntime(c.NetworkID); err != nil {
		return fmt.Errorf("Master: %w", err)
	}
	dirs, ipcEndpoints := make(map[string]bool), make(map[string]bool)
	tcpEndpoints := make(map[uint64][]*net.TCPAddr)
	for i, cfg := range append([]NodeConfig{c.Master}, c.Slaves...) {
		if i > 0 {
			if err := cfg.validateRuntime(c.NetworkID); err != nil {
				return fmt.Errorf("Slaves[%d]: %w", i-1, err)
			}
		}
		// Geth opens Auth RPC only for registered authenticated APIs; QKC
		// does not register any yet, so its Auth endpoint is not reserved here.
		endpoints := []string{cfg.PrivateEndpoint(), cfg.Node.P2P.ListenAddr, cfg.Node.WSEndpoint()}
		if cfg.Node.WSHost == "" || cfg.Node.HTTPPort != cfg.Node.WSPort {
			// Geth shares the listener and uses WSHost when HTTP and WS ports match.
			endpoints = append(endpoints, cfg.Node.HTTPEndpoint())
		}
		for _, address := range endpoints {
			if address == "" {
				continue // Disabled listener.
			}
			// Runtime validation has already checked the address and port.
			host, port, _ := net.SplitHostPort(address)
			value, _ := strconv.ParseUint(port, 10, 16)
			if value == 0 {
				continue // Dynamically assigned ports cannot conflict here.
			}
			// Resolve aliases using the same address selection as net.Listen.
			endpoint, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(canonicalHost(host), port))
			if err != nil {
				return fmt.Errorf("resolve TCP listener %q: %w", address, err)
			}
			ip := endpoint.AddrPort().Addr().Unmap()
			for _, other := range tcpEndpoints[value] {
				otherIP := other.AddrPort().Addr().Unmap()
				if ip == otherIP || !ip.IsValid() || !otherIP.IsValid() || ip.IsUnspecified() || otherIP.IsUnspecified() {
					return fmt.Errorf("TCP listeners conflict: %s and %s", address, other)
				}
			}
			tcpEndpoints[value] = append(tcpEndpoints[value], endpoint)
		}
		if cfg.Node.DataDir != "" {
			dir, err := filepath.Abs(cfg.Node.DataDir)
			if err != nil {
				return err
			}
			if dirs[dir] {
				return fmt.Errorf("process data directory is shared: %s", cfg.Node.DataDir)
			}
			dirs[dir] = true
		}
		if endpoint := cfg.Node.IPCEndpoint(); endpoint != "" {
			path, err := filepath.Abs(endpoint)
			if err != nil {
				return err
			}
			if runtime.GOOS == "windows" {
				path = strings.ToLower(path) // Windows named pipes are case-insensitive.
			}
			if ipcEndpoints[path] {
				return fmt.Errorf("process IPC endpoint is shared: %s", endpoint)
			}
			ipcEndpoints[path] = true
		}
	}
	return nil
}

// ValidateGenesis checks loaded genesis configurations and network membership.
// It does not load files, modify configurations, or inspect process settings.
func (c *ClusterConfig) ValidateGenesis() error {
	if c.Master.Role != RoleMaster || c.Master.ChainID != nil {
		return errors.New("Master must not specify ChainID")
	}
	if err := c.Master.ValidateGenesis(); err != nil {
		return fmt.Errorf("Master: %w", err)
	}
	if len(c.Slaves) == 0 {
		return errors.New("deployment must define at least one shard")
	}
	var first *params.ChainConfig
	ids := make(map[string]bool, len(c.Slaves))
	for i, cfg := range c.Slaves {
		if cfg.Role != RoleSlave {
			return fmt.Errorf("Slaves[%d]: invalid process role %q", i, cfg.Role)
		}
		if cfg.ChainID == nil {
			return fmt.Errorf("Slaves[%d]: ChainID is required", i)
		}
		id := cfg.ChainID.String()
		if ids[id] {
			return fmt.Errorf("duplicate deployed chain ID %v", cfg.ChainID)
		}
		if err := cfg.ValidateGenesis(); err != nil {
			return fmt.Errorf("Slaves[%d]: %w", i, err)
		}
		ids[id] = true
		chain := cfg.Eth.Genesis.Config
		if first == nil {
			first = chain
		} else {
			if !slices.EqualFunc(first.QuarkChain.ChainIDs, chain.QuarkChain.ChainIDs, func(a, b *big.Int) bool { return a.Cmp(b) == 0 }) {
				return fmt.Errorf("chain %v: genesis chain IDs must match in order across shards", chain.ChainID)
			}
			if err := checkSharedForks(first, chain); err != nil {
				return err
			}
		}
	}
	if len(ids) != len(first.QuarkChain.ChainIDs) {
		return errors.New("deployment must contain every genesis chain ID")
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

// checkSharedForks preserves the shared activation heights across independently
// supplied genesis files. It never fills or overwrites either chain's settings.
func checkSharedForks(first, other *params.ChainConfig) error {
	for _, fork := range []struct {
		name string
		a, b *big.Int
	}{
		{"HomesteadBlock", first.HomesteadBlock, other.HomesteadBlock},
		{"EIP150Block", first.EIP150Block, other.EIP150Block},
		{"EIP155Block", first.EIP155Block, other.EIP155Block},
		{"EIP158Block", first.EIP158Block, other.EIP158Block},
		{"ByzantiumBlock", first.ByzantiumBlock, other.ByzantiumBlock},
		{"ConstantinopleBlock", first.ConstantinopleBlock, other.ConstantinopleBlock},
		{"PetersburgBlock", first.PetersburgBlock, other.PetersburgBlock},
		{"IstanbulBlock", first.IstanbulBlock, other.IstanbulBlock},
		{"BerlinBlock", first.BerlinBlock, other.BerlinBlock},
		{"LondonBlock", first.LondonBlock, other.LondonBlock},
	} {
		if (fork.a == nil) != (fork.b == nil) || (fork.a != nil && fork.b != nil && fork.a.Cmp(fork.b) != 0) {
			return fmt.Errorf("chain %v: genesis fork %s must match chain %v", other.ChainID, fork.name, first.ChainID)
		}
	}
	return nil
}

// checkShardChainID validates the EVM identifier used for execution and routing.
func checkShardChainID(id *big.Int) error {
	if id == nil || id.Sign() <= 0 || id.BitLen() > 256 {
		return errors.New("ChainID must be a positive uint256 for EVM execution and routing")
	}
	return nil
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

// Validate checks loaded genesis rules and process settings. Only shard execution
// settings bind Eth.NetworkId to the deployment's NetworkID.
func (c *NodeConfig) Validate(networkID uint64) error {
	if networkID == 0 {
		return errors.New("NetworkID must be positive; it is not inferred from an EVM chain ID")
	}
	if err := c.ValidateGenesis(); err != nil {
		return err
	}
	return c.validateRuntime(networkID)
}

// ValidateGenesis checks this process's loaded chain identity and consensus rules.
// It does not load files, modify configurations, or inspect runtime settings.
func (c *NodeConfig) ValidateGenesis() error {
	switch c.Role {
	case RoleMaster:
		if c.Root == nil {
			return errors.New("root Genesis config is required; set GenesisFile")
		}
		if err := c.Root.ConsensusType.Check(); err != nil {
			return err
		}
		if err := c.Root.PoSW.Check(); err != nil {
			return err
		}
	case RoleSlave:
		if c.ChainID == nil || c.Eth == nil {
			return errors.New("slave requires ChainID and execution settings")
		}
		if err := checkShardChainID(c.ChainID); err != nil {
			return err
		}
		if c.Eth.Genesis == nil || c.Eth.Genesis.Config == nil {
			return errors.New("shard Genesis config is required; set GenesisFile")
		}
		chain := c.Eth.Genesis.Config
		if err := checkShardChainID(chain.ChainID); err != nil {
			return fmt.Errorf("Genesis: %w", err)
		}
		if chain.ChainID.Cmp(c.ChainID) != 0 {
			return fmt.Errorf("Genesis ChainID %v does not match deployed ChainID %v", chain.ChainID, c.ChainID)
		}
		if chain.QuarkChain == nil {
			return errors.New("Genesis QuarkChain configuration is required")
		}
		if err := chain.QuarkChain.ConsensusType.Check(); err != nil {
			return err
		}
		if err := chain.CheckConfigForkOrder(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid process role %q", c.Role)
	}
	return nil
}

func (c *NodeConfig) validateRuntime(networkID uint64) error {
	if networkID == 0 {
		return errors.New("NetworkID must be positive; it is not inferred from an EVM chain ID")
	}
	switch c.Role {
	case RoleMaster:
		if c.Eth != nil || c.ChainID != nil || c.PrivateHost != "" || c.PrivatePort != 0 {
			return errors.New("master cannot configure Eth, ChainID, PrivateHost or PrivatePort")
		}
	case RoleSlave:
		if c.ChainID == nil || c.Eth == nil {
			return errors.New("slave requires ChainID and execution settings")
		}
		if err := checkShardChainID(c.ChainID); err != nil {
			return err
		}
		ip, err := netip.ParseAddr(c.PrivateHost)
		if c.PrivateHost == "" || strings.ContainsAny(c.PrivateHost, " \t\r\n/[]") ||
			(strings.Contains(c.PrivateHost, ":") && err != nil) || (err == nil && ip.Unmap().IsUnspecified()) {
			return fmt.Errorf("PrivateHost must be a concrete host without brackets or a port: %q", c.PrivateHost)
		}
		if c.PrivatePort < 1 || c.PrivatePort > 65535 {
			return errors.New("PrivatePort must be between 1 and 65535")
		}
		p := c.Node.P2P
		if p.ListenAddr != "" || p.DiscAddr != "" || p.MaxPeers != 0 || !p.NoDiscovery || !p.NoDial ||
			p.DiscoveryV4 || p.DiscoveryV5 || len(p.BootstrapNodes)+len(p.BootstrapNodesV5)+len(p.StaticNodes)+len(p.TrustedNodes) != 0 {
			return errors.New("slave external P2P must be disabled; configure PrivateHost and PrivatePort instead")
		}
	default:
		return fmt.Errorf("invalid process role %q", c.Role)
	}
	if c.Eth != nil {
		if c.Eth.NetworkId != 0 && c.Eth.NetworkId != networkID {
			return fmt.Errorf("Eth.NetworkId %d conflicts with NetworkID %d", c.Eth.NetworkId, networkID)
		}
		if c.Eth.OverrideOsaka != nil || c.Eth.OverrideAmsterdam != nil || c.Eth.OverrideBPO1 != nil || c.Eth.OverrideBPO2 != nil || c.Eth.OverrideUBT != nil {
			return errors.New("local fork overrides are not allowed; configure forks in Genesis")
		}
		if c.Eth.StateScheme != "" && c.Eth.StateScheme != "hash" && c.Eth.StateScheme != "path" {
			return fmt.Errorf("invalid Eth.StateScheme %q", c.Eth.StateScheme)
		}
		if !c.Eth.SyncMode.IsValid() {
			return fmt.Errorf("invalid Eth.SyncMode %d", c.Eth.SyncMode)
		}
		for name, value := range map[string]int{"Eth.DatabaseCache": c.Eth.DatabaseCache, "Eth.TrieCleanCache": c.Eth.TrieCleanCache, "Eth.TrieDirtyCache": c.Eth.TrieDirtyCache, "Eth.SnapshotCache": c.Eth.SnapshotCache} {
			if value < 0 {
				return fmt.Errorf("%s must not be negative", name)
			}
		}
		c.Eth.NetworkId = networkID
	}
	if c.Node.DBEngine != "" && c.Node.DBEngine != "pebble" && c.Node.DBEngine != "leveldb" {
		return fmt.Errorf("invalid Node.DBEngine %q", c.Node.DBEngine)
	}
	for name, value := range map[string]int{
		"Node.P2P.MaxPeers": c.Node.P2P.MaxPeers, "Node.P2P.MaxPendingPeers": c.Node.P2P.MaxPendingPeers,
	} {
		if value < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	for name, port := range map[string]int{"PublicPort": c.Node.HTTPPort, "Node.WSPort": c.Node.WSPort, "Node.AuthPort": c.Node.AuthPort} {
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
	// macOS has a 104-byte Unix socket path, including the terminator.
	if endpoint := c.Node.IPCEndpoint(); runtime.GOOS != "windows" && len(endpoint) > 103 {
		return fmt.Errorf("IPC endpoint exceeds Unix socket path limit: %s", endpoint)
	}
	return nil
}
