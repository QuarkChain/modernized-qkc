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
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	qkcconfig "github.com/ethereum/go-ethereum/qkc/config"
	"github.com/urfave/cli/v2"
)

const qkcExampleNode = "../../qkc/config/examples/node.toml"
const qkcExampleNetwork = "../../qkc/config/examples/network.toml"

func readQKCFlags(args ...string) (*qkcconfig.NodeConfig, error) {
	savedDataDirFlag := *utils.DataDirFlag
	defer func() { *utils.DataDirFlag = savedDataDirFlag }()
	var cfg *qkcconfig.NodeConfig
	app := &cli.App{
		Flags: append(slices.Clone(checkQKCConfigCommand.Flags), utils.MainnetFlag, utils.CacheFlag),
		Action: func(ctx *cli.Context) (err error) {
			cfg, err = loadQKCConfig(ctx)
			return err
		},
	}
	err := app.Run(append([]string{"geth"}, args...))
	return cfg, err
}

func TestQKCConfigPrecedence(t *testing.T) {
	cfg, err := readQKCFlags("--config", qkcExampleNode)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.P2P.MaxPeers != 25 || cfg.Eth.DatabaseCache != 512 || cfg.Eth.SyncMode != ethconfig.FullSync || cfg.Node.HTTPHost != "127.0.0.1" {
		t.Fatal("unset CLI flags replaced configured values")
	}
	if cfg.Eth.TrieCleanCache != ethconfig.Defaults.TrieCleanCache || cfg.Eth.NetworkId != 2 {
		t.Fatal("defaults or network ID resolved incorrectly")
	}
	if cfg.NetworkFile != filepath.Clean(qkcExampleNetwork) {
		t.Fatalf("network file resolved incorrectly: %s", cfg.NetworkFile)
	}

	cfg, err = readQKCFlags("--config", qkcExampleNode,
		"--maxpeers", "0", "--nodiscover=false", "--http=false", "--ws", "--ws.addr", "127.0.0.2", "--ws.port", "0",
		"--datadir", "./cli-data", "--syncmode", "snap", "--rpc.gascap", "0", "--rpc.evmtimeout", "2s", "--rpc.txfeecap", "0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.P2P.MaxPeers != 0 || cfg.Node.P2P.NoDiscovery || cfg.Node.HTTPHost != "" || cfg.Node.WSHost != "127.0.0.2" || cfg.Node.WSPort != 0 {
		t.Fatal("explicit zero or false CLI values did not override local TOML")
	}
	if cfg.Node.DataDir != "cli-data" || cfg.Eth.SyncMode != ethconfig.SnapSync || cfg.Eth.RPCGasCap != 0 || cfg.Eth.RPCTxFeeCap != 0 || cfg.Eth.RPCEVMTimeout != 2*time.Second {
		t.Fatalf("explicit CLI values were not applied: datadir=%q sync=%v gascap=%d feecap=%v timeout=%v", cfg.Node.DataDir, cfg.Eth.SyncMode, cfg.Eth.RPCGasCap, cfg.Eth.RPCTxFeeCap, cfg.Eth.RPCEVMTimeout)
	}

	// A CLI network path is relative to cwd and overrides the local file's path.
	file := filepath.Join(t.TempDir(), "node.toml")
	if err := os.WriteFile(file, []byte("NetworkFile = \"missing.toml\"\n[Eth]\nNetworkId = 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = readQKCFlags("--config", file, "--qkc.network", qkcExampleNetwork, "--networkid", "2")
	if err != nil || cfg.Eth.NetworkId != 2 {
		t.Fatalf("CLI did not override local network settings: %v", err)
	}
}

func TestQKCEndpointOverrides(t *testing.T) {
	for _, test := range []struct {
		args []string
		http string
		ws   string
	}{
		{[]string{"--http.addr", "127.0.0.2"}, "127.0.0.2", ""},
		{[]string{"--ws.addr", "127.0.0.2"}, "127.0.0.1", ""},
		{[]string{"--http=false", "--http.addr", "127.0.0.2"}, "", ""},
		{[]string{"--ws"}, "127.0.0.1", "localhost"},
	} {
		cfg, err := readQKCFlags(append([]string{"--config", qkcExampleNode}, test.args...)...)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Node.HTTPHost != test.http || cfg.Node.WSHost != test.ws {
			t.Errorf("%v: endpoints = %q, %q; want %q, %q", test.args, cfg.Node.HTTPHost, cfg.Node.WSHost, test.http, test.ws)
		}
	}
}

func TestQKCConfigInvalidFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--networkid", "100001"}, "conflicts with NetworkID"},
		{[]string{"--networkid", "0"}, "must match the positive NetworkID"},
		{[]string{"--mainnet"}, "--mainnet is not supported"},
		{[]string{"--cache", "100"}, "--cache is not supported"},
		{[]string{"--ipcdisable", "--ipcpath", "qkc.ipc"}, "mutually exclusive"},
		{[]string{"--maxpeers", "-1"}, "MaxPeers must not be negative"},
		{[]string{"--http.port", "65536"}, "HTTPPort must be between"},
		{[]string{"--port", "-1"}, "invalid port"},
		{[]string{"--db.engine", "unknown"}, "invalid Node.DBEngine"},
		{[]string{"--syncmode", "unknown"}, "unknown sync mode"},
		{[]string{"--state.scheme", "unknown"}, "invalid Eth.StateScheme"},
		{[]string{"extra"}, "no positional arguments"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, err := readQKCFlags(append([]string{"--config", qkcExampleNode}, test.args...)...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
	if _, err := readQKCFlags(); err == nil {
		t.Fatal("missing network file accepted")
	}
}

func TestCheckQKCConfig(t *testing.T) {
	savedDataDirFlag := *utils.DataDirFlag
	t.Cleanup(func() { *utils.DataDirFlag = savedDataDirFlag })
	var out bytes.Buffer
	datadir := filepath.Join(t.TempDir(), "unused")
	app := &cli.App{Writer: &out, Commands: []*cli.Command{checkQKCConfigCommand}}
	if err := app.Run([]string{"geth", "checkqkcconfig", "--qkc.network", qkcExampleNetwork, "--datadir", datadir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(datadir); !os.IsNotExist(err) {
		t.Fatalf("configuration check created the data directory: %v", err)
	}
	if out.String() != "QuarkChain configuration is valid (network ID 2)\n" {
		t.Fatalf("unexpected validation output: %s", &out)
	}
}
