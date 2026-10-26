// Copyright 2026-2027 The quarkchain Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/internal/debug"
	"github.com/ethereum/go-ethereum/node"
	qkcconfig "github.com/ethereum/go-ethereum/qkc/config"
	"github.com/urfave/cli/v2"
)

var qkcNetworkFileFlag = &cli.StringFlag{
	Name:  "qkc.network",
	Usage: "QKC network TOML file (overrides NetworkFile in the local configuration)",
}

var qkcConfigFlags = []cli.Flag{
	configFileFlag, qkcNetworkFileFlag, utils.NetworkIdFlag,
	utils.DataDirFlag, utils.DBEngineFlag, utils.SyncModeFlag, utils.StateSchemeFlag,
	utils.ListenPortFlag, utils.MaxPeersFlag, utils.NoDiscoverFlag,
	utils.HTTPEnabledFlag, utils.HTTPListenAddrFlag, utils.HTTPPortFlag, utils.HTTPApiFlag,
	utils.WSEnabledFlag, utils.WSListenAddrFlag, utils.WSPortFlag, utils.WSApiFlag,
	utils.IPCDisabledFlag, utils.IPCPathFlag,
	utils.RPCGlobalGasCapFlag, utils.RPCGlobalEVMTimeoutFlag, utils.RPCGlobalTxFeeCapFlag,
}

var checkQKCConfigCommand = &cli.Command{
	Name:   "checkqkcconfig",
	Usage:  "Validate QKC network and local settings without starting the cluster",
	Flags:  qkcConfigFlags,
	Action: checkQKCConfig,
}

func loadQKCConfig(ctx *cli.Context) (*qkcconfig.NodeConfig, error) {
	if ctx.NArg() != 0 {
		return nil, fmt.Errorf("checkqkcconfig takes no positional arguments")
	}
	// The parent geth app also accepts Ethereum flags. Reject unsupported flags
	// rather than silently ignoring a requested setting or network preset.
	allowed := make(map[string]bool)
	for _, flag := range slices.Concat(qkcConfigFlags, debug.Flags) {
		for _, name := range flag.Names() {
			allowed[name] = true
		}
	}
	for _, name := range ctx.FlagNames() {
		if ctx.IsSet(name) && !allowed[name] {
			return nil, fmt.Errorf("--%s is not supported by checkqkcconfig", name)
		}
	}
	cfg, err := qkcconfig.LoadNode(ctx.String(configFileFlag.Name))
	if err != nil {
		return nil, err
	}
	if ctx.IsSet(qkcNetworkFileFlag.Name) {
		cfg.NetworkFile = ctx.String(qkcNetworkFileFlag.Name)
	}
	if cfg.NetworkFile == "" {
		return nil, fmt.Errorf("set NetworkFile in --config or supply --qkc.network")
	}
	if ctx.IsSet(utils.NetworkIdFlag.Name) {
		cfg.Eth.NetworkId = ctx.Uint64(utils.NetworkIdFlag.Name)
		if cfg.Eth.NetworkId == 0 {
			return nil, fmt.Errorf("--networkid must match the positive NetworkID in the network file")
		}
	}
	if ctx.IsSet(utils.DataDirFlag.Name) {
		cfg.Node.DataDir = ctx.String(utils.DataDirFlag.Name)
	}
	if ctx.IsSet(utils.DBEngineFlag.Name) {
		cfg.Node.DBEngine = ctx.String(utils.DBEngineFlag.Name)
	}
	if ctx.IsSet(utils.SyncModeFlag.Name) {
		if err := cfg.Eth.SyncMode.UnmarshalText([]byte(ctx.String(utils.SyncModeFlag.Name))); err != nil {
			return nil, err
		}
	}
	if ctx.IsSet(utils.StateSchemeFlag.Name) {
		cfg.Eth.StateScheme = ctx.String(utils.StateSchemeFlag.Name)
	}
	if ctx.IsSet(utils.ListenPortFlag.Name) {
		cfg.Node.P2P.ListenAddr = fmt.Sprintf(":%d", ctx.Int(utils.ListenPortFlag.Name))
	}
	if ctx.IsSet(utils.MaxPeersFlag.Name) {
		cfg.Node.P2P.MaxPeers = ctx.Int(utils.MaxPeersFlag.Name)
	}
	if ctx.IsSet(utils.NoDiscoverFlag.Name) {
		cfg.Node.P2P.NoDiscovery = ctx.Bool(utils.NoDiscoverFlag.Name)
	}
	if ctx.IsSet(utils.HTTPPortFlag.Name) {
		cfg.Node.HTTPPort = ctx.Int(utils.HTTPPortFlag.Name)
	}
	if ctx.IsSet(utils.HTTPApiFlag.Name) {
		cfg.Node.HTTPModules = utils.SplitAndTrim(ctx.String(utils.HTTPApiFlag.Name))
	}
	if ctx.IsSet(utils.WSPortFlag.Name) {
		cfg.Node.WSPort = ctx.Int(utils.WSPortFlag.Name)
	}
	if ctx.IsSet(utils.WSApiFlag.Name) {
		cfg.Node.WSModules = utils.SplitAndTrim(ctx.String(utils.WSApiFlag.Name))
	}
	applyQKCEndpoint(ctx, utils.HTTPEnabledFlag.Name, utils.HTTPListenAddrFlag.Name, &cfg.Node.HTTPHost, node.DefaultHTTPHost)
	applyQKCEndpoint(ctx, utils.WSEnabledFlag.Name, utils.WSListenAddrFlag.Name, &cfg.Node.WSHost, node.DefaultWSHost)
	if ctx.IsSet(utils.IPCDisabledFlag.Name) && ctx.IsSet(utils.IPCPathFlag.Name) {
		return nil, fmt.Errorf("--ipcdisable and --ipcpath are mutually exclusive")
	}
	if ctx.Bool(utils.IPCDisabledFlag.Name) {
		cfg.Node.IPCPath = ""
	}
	if ctx.IsSet(utils.IPCPathFlag.Name) {
		cfg.Node.IPCPath = ctx.String(utils.IPCPathFlag.Name)
	}
	if ctx.IsSet(utils.RPCGlobalGasCapFlag.Name) {
		cfg.Eth.RPCGasCap = ctx.Uint64(utils.RPCGlobalGasCapFlag.Name)
	}
	if ctx.IsSet(utils.RPCGlobalEVMTimeoutFlag.Name) {
		cfg.Eth.RPCEVMTimeout = ctx.Duration(utils.RPCGlobalEVMTimeoutFlag.Name)
	}
	if ctx.IsSet(utils.RPCGlobalTxFeeCapFlag.Name) {
		cfg.Eth.RPCTxFeeCap = ctx.Float64(utils.RPCGlobalTxFeeCapFlag.Name)
	}
	network, err := qkcconfig.LoadNetwork(cfg.NetworkFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(network); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Explicit false disables an endpoint configured in TOML; an address alone
// changes the address without enabling an endpoint that was disabled.
func applyQKCEndpoint(ctx *cli.Context, enabled, address string, host *string, defaultHost string) {
	if ctx.IsSet(address) && *host != "" {
		*host = ctx.String(address)
	}
	if ctx.IsSet(enabled) {
		if !ctx.Bool(enabled) {
			*host = ""
		} else if ctx.IsSet(address) {
			*host = ctx.String(address)
		} else if *host == "" {
			*host = defaultHost
		}
	}
}

func checkQKCConfig(ctx *cli.Context) error {
	cfg, err := loadQKCConfig(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(ctx.App.Writer, "QuarkChain configuration is valid (network ID %d)\n", cfg.Eth.NetworkId)
	return err
}
