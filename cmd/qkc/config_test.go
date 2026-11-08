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
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	qkcconfig "github.com/ethereum/go-ethereum/qkc/config"
	"github.com/urfave/cli/v2"
)

const qkcExampleCluster = "../../qkc/config/examples/cluster.toml"

func copyQKCExample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "genesis"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cluster.toml", "genesis/root.json", "genesis/shard-100001.json", "genesis/shard-100002.json"} {
		content, err := os.ReadFile(filepath.Join(filepath.Dir(qkcExampleCluster), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "cluster.toml")
}

func TestStandaloneSlaveLocalGenesis(t *testing.T) {
	file := copyQKCExample(t)
	for _, name := range []string{"root.json", "shard-100002.json"} {
		if err := os.Remove(filepath.Join(filepath.Dir(file), "genesis", name)); err != nil {
			t.Fatal(err)
		}
	}
	app := newApp()
	app.Before, app.After = nil, nil
	app.Action = func(ctx *cli.Context) error {
		stack, err := makeSlaveNode(ctx)
		if err != nil {
			return err
		}
		defer stack.Close()
		if err := stack.Start(); err != nil {
			return err
		}
		client := stack.Attach()
		defer client.Close()
		var id hexutil.Big
		if err := client.CallContext(t.Context(), &id, "eth_chainId"); err != nil {
			return err
		}
		if id.ToInt().Int64() != 100001 {
			t.Fatalf("unexpected chain ID: %v", &id)
		}
		return nil
	}
	if err := app.Run([]string{"qkc", "--config", file, "--qkc.role", "slave", "--qkc.chain", "100001", "--datadir", t.TempDir(), "--http=false", "--ipcdisable"}); err != nil {
		t.Fatal(err)
	}
}

func readQKCFlags(args ...string) (*qkcconfig.NodeConfig, error) {
	savedDataDirFlag := *utils.DataDirFlag
	defer func() { *utils.DataDirFlag = savedDataDirFlag }()
	var cfg *qkcconfig.NodeConfig
	app := &cli.App{
		Flags: checkQKCConfigCommand.Flags,
		Action: func(ctx *cli.Context) (err error) {
			var cluster *qkcconfig.ClusterConfig
			cfg, cluster, err = loadQKCConfig(ctx)
			if err != nil {
				return err
			}
			if err := cluster.LoadGenesis(); err != nil {
				return err
			}
			return cluster.Validate()
		},
	}
	err := app.Run(append([]string{"qkc"}, args...))
	return cfg, err
}

func TestQKCConfigPrecedence(t *testing.T) {
	cfg, err := readQKCFlags("--config", qkcExampleCluster, "--qkc.role", "slave", "--qkc.chain", "100001")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.P2P.MaxPeers != 0 || cfg.Eth.DatabaseCache != 512 || cfg.Node.HTTPHost != "127.0.0.1" {
		t.Fatal("unset CLI flags replaced configured values")
	}
	if cfg.PrivateHost != "127.0.0.1" || cfg.PrivatePort != 38000 {
		t.Fatal("cluster host and port were not preserved")
	}
	if cfg.CoinbaseAddress != common.HexToAddress("0x2222222222222222222222222222222222222222") {
		t.Fatal("configured shard mining address was not preserved")
	}
	if cfg.Eth.TrieCleanCache != ethconfig.Defaults.TrieCleanCache || cfg.Eth.SyncMode != ethconfig.Defaults.SyncMode || cfg.Eth.RPCGasCap != ethconfig.Defaults.RPCGasCap || cfg.Eth.NetworkId != 2 {
		t.Fatal("defaults or network ID resolved incorrectly")
	}
	if want := filepath.Join(filepath.Dir(qkcExampleCluster), "genesis/shard-100001.json"); cfg.GenesisFile != want {
		t.Fatalf("genesis file = %s, want %s", cfg.GenesisFile, want)
	}
	if cfg.Eth.Genesis == nil || cfg.Eth.Genesis.Config.ChainID.Cmp(cfg.ChainID) != 0 {
		t.Fatal("selected shard genesis was not loaded")
	}

	cfg, err = readQKCFlags("--config", qkcExampleCluster, "--qkc.role", "slave", "--qkc.chain", "100001",
		"--http=false", "--http.port", "28545", "--ws", "--ws.addr", "127.0.0.2", "--ws.port", "0",
		"--datadir", "./cli-data", "--syncmode", "full", "--rpc.gascap", "0", "--rpc.evmtimeout", "2s", "--rpc.txfeecap", "0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.HTTPHost != "" || cfg.Node.HTTPPort != 28545 || cfg.Node.WSHost != "127.0.0.2" || cfg.Node.WSPort != 0 {
		t.Fatal("explicit zero or false CLI values did not override local TOML")
	}
	if cfg.PrivateHost != "127.0.0.1" || cfg.PrivatePort != 38000 {
		t.Fatal("RPC overrides affected the internal cluster endpoint")
	}
	if cfg.Node.DataDir != "cli-data" || cfg.Eth.SyncMode != ethconfig.FullSync || cfg.Eth.RPCGasCap != 0 || cfg.Eth.RPCTxFeeCap != 0 || cfg.Eth.RPCEVMTimeout != 2*time.Second {
		t.Fatalf("explicit CLI values were not applied: datadir=%q sync=%v gascap=%d feecap=%v timeout=%v", cfg.Node.DataDir, cfg.Eth.SyncMode, cfg.Eth.RPCGasCap, cfg.Eth.RPCTxFeeCap, cfg.Eth.RPCEVMTimeout)
	}

	master, err := readQKCFlags("--config", qkcExampleCluster, "--qkc.role", "master", "--maxpeers", "0", "--nodiscover=false")
	if err != nil {
		t.Fatal(err)
	}
	if master.Eth != nil || master.Node.P2P.MaxPeers != 0 || master.Node.P2P.NoDiscovery || len(master.ShardEndpoints) != 2 || master.ShardEndpoints["100001"] != "127.0.0.1:38000" {
		t.Fatal("master selection, overrides or derived endpoints are incorrect")
	}
	if master.CoinbaseAddress != common.HexToAddress("0x1111111111111111111111111111111111111111") {
		t.Fatal("CLI overrides replaced the master mining address")
	}
}

func TestQKCLargeChainIDSelection(t *testing.T) {
	const chainID = "18446744073709551617" // Greater than uint64.
	clusterFile := copyQKCExample(t)
	cluster, err := qkcconfig.LoadCluster(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	for _, slave := range cluster.Slaves {
		config := slave.Eth.Genesis.Config
		if config.ChainID.Cmp(big.NewInt(100001)) == 0 {
			config.ChainID.SetString(chainID, 10)
		}
		for _, id := range config.QuarkChain.ChainIDs {
			if id.Cmp(big.NewInt(100001)) == 0 {
				id.SetString(chainID, 10)
			}
		}
		content, err := json.Marshal(slave.Eth.Genesis)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(slave.GenesisFile, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	content = bytes.Replace(content, []byte(`ChainID = "100001"`), []byte(`ChainID = "`+chainID+`"`), 1)
	if err := os.WriteFile(clusterFile, content, 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := readQKCFlags("--config", clusterFile, "--qkc.role", "slave", "--qkc.chain", chainID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ChainID == nil || selected.ChainID.String() != chainID {
		t.Fatalf("selected chain ID = %v, want %s", selected.ChainID, chainID)
	}
	master, err := readQKCFlags("--config", clusterFile, "--qkc.role", "master")
	if err != nil {
		t.Fatal(err)
	}
	if master.ShardEndpoints[chainID] != "127.0.0.1:38000" {
		t.Fatalf("large chain endpoint = %q", master.ShardEndpoints[chainID])
	}
}

func TestQKCNetworkIDOverride(t *testing.T) {
	file := copyQKCExample(t)
	input, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(string(input), "[Slaves.Eth]", "[Slaves.Eth]\nNetworkId = 99", 1)
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readQKCFlags("--config", file, "--qkc.role", "slave", "--qkc.chain", "100001", "--networkid", "2")
	if err != nil || cfg.Eth.NetworkId != 2 {
		t.Fatalf("CLI did not override local network settings: %v", err)
	}
	// An override must not repair the conflicting NetworkId of another process.
	_, err = readQKCFlags("--config", file, "--qkc.role", "slave", "--qkc.chain", "100002", "--networkid", "2")
	if err == nil || !strings.Contains(err.Error(), "Slaves[0]: Eth.NetworkId 99 conflicts") {
		t.Fatalf("CLI overrides affected an unselected process: %v", err)
	}
}

func TestQKCUint64NetworkID(t *testing.T) {
	for _, test := range []struct {
		input string
		want  uint64
	}{
		{"4294967296", 1 << 32},
		{"18446744073709551615", ^uint64(0)},
	} {
		t.Run(test.input, func(t *testing.T) {
			file := copyQKCExample(t)
			input, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			input = bytes.Replace(input, []byte("NetworkID = 2"), []byte("NetworkID = "+test.input), 1)
			if err := os.WriteFile(file, input, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := readQKCFlags("--config", file, "--qkc.role", "slave", "--qkc.chain", "100001", "--networkid", test.input)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Eth.NetworkId != test.want {
				t.Fatalf("network ID = %d, want %d", cfg.Eth.NetworkId, test.want)
			}
		})
	}
}

func TestQKCSelectedGenesisIgnoresBrokenPeer(t *testing.T) {
	savedDataDirFlag := *utils.DataDirFlag
	t.Cleanup(func() { *utils.DataDirFlag = savedDataDirFlag })
	for _, test := range []struct {
		name      string
		breakFile func(string) error
	}{
		{"missing", os.Remove},
		{"corrupt", func(file string) error { return os.WriteFile(file, []byte("{"), 0600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := copyQKCExample(t)
			if err := test.breakFile(filepath.Join(filepath.Dir(file), "genesis/shard-100002.json")); err != nil {
				t.Fatal(err)
			}
			var cfg *qkcconfig.NodeConfig
			var cluster *qkcconfig.ClusterConfig
			app := &cli.App{
				Flags: checkQKCConfigCommand.Flags,
				Action: func(ctx *cli.Context) (err error) {
					cfg, cluster, err = loadQKCConfig(ctx)
					return err
				},
			}
			args := []string{"--config", file, "--qkc.role", "slave", "--qkc.chain", "100001"}
			if err := app.Run(append([]string{"qkc"}, args...)); err != nil {
				t.Fatalf("selecting a process read a peer's genesis: %v", err)
			}
			if cfg.Eth.Genesis != nil || cluster.Slaves[1].Eth.Genesis != nil {
				t.Fatal("loading process settings also loaded genesis")
			}
			if err := cfg.LoadGenesis(); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(cluster.NetworkID); err != nil {
				t.Fatalf("selected process depends on a peer's genesis: %v", err)
			}
			check := newApp()
			var out bytes.Buffer
			check.Writer = &out
			if err := check.Run(append([]string{"qkc", "checkconfig"}, args...)); err == nil || !strings.Contains(err.Error(), "shard-100002.json") {
				t.Fatalf("checkconfig did not reject the broken peer genesis: %v", err)
			}
		})
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
		{[]string{"--http=false", "--http.port", "18546"}, "", ""},
		{[]string{"--ws"}, "127.0.0.1", "localhost"},
	} {
		cfg, err := readQKCFlags(append([]string{"--config", qkcExampleCluster, "--qkc.role", "slave", "--qkc.chain", "100001"}, test.args...)...)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Node.HTTPHost != test.http || cfg.Node.WSHost != test.ws {
			t.Errorf("%v: endpoints = %q, %q; want %q, %q", test.args, cfg.Node.HTTPHost, cfg.Node.WSHost, test.http, test.ws)
		}
	}
}

func TestQKCConfigInvalidFlags(t *testing.T) {
	cluster, err := qkcconfig.LoadCluster(qkcExampleCluster)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--networkid", "100001"}, "must match the positive NetworkID"},
		{[]string{"--networkid", "0"}, "must match the positive NetworkID"},
		{[]string{"--qkc.network", "network.toml"}, "flag provided but not defined: -qkc.network"},
		{[]string{"--mainnet"}, "flag provided but not defined: -mainnet"},
		{[]string{"--cache", "100"}, "flag provided but not defined: -cache"},
		{[]string{"--ipcdisable", "--ipcpath", "qkc.ipc"}, "mutually exclusive"},
		{[]string{"--maxpeers", "-1"}, "external P2P must be disabled"},
		{[]string{"--http.port", "65536"}, "PublicPort must be between"},
		{[]string{"--http.port", "18546"}, "TCP listeners conflict"},
		{[]string{"--http.port", "38000"}, "TCP listeners conflict"},
		{[]string{"--http.port", "30303"}, "TCP listeners conflict"},
		{[]string{"--ws", "--ws.port", "38000"}, "TCP listeners conflict"},
		{[]string{"--ws", "--ws.addr", "127.0.0.1", "--ws.port", "38000"}, "TCP listeners conflict"},
		{[]string{"--ipcpath", cluster.Master.Node.IPCEndpoint()}, "IPC endpoint is shared"},
		{[]string{"--port", "30304"}, "external P2P must be disabled"},
		{[]string{"--db.engine", "unknown"}, "invalid Node.DBEngine"},
		{[]string{"--syncmode", "unknown"}, "unknown sync mode"},
		{[]string{"--state.scheme", "unknown"}, "invalid Eth.StateScheme"},
		{[]string{"extra"}, "no positional arguments"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, err := readQKCFlags(append([]string{"--config", qkcExampleCluster, "--qkc.role", "slave", "--qkc.chain", "100001"}, test.args...)...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
	if _, err := readQKCFlags(); err == nil {
		t.Fatal("missing deployment accepted")
	}
}

func TestQKCProcessFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "set --qkc.role"},
		{[]string{"--qkc.role", "shard"}, "set --qkc.role"},
		{[]string{"--qkc.role", "master", "--qkc.chain", "100001"}, "master cannot select"},
		{[]string{"--qkc.role", "slave"}, "requires a chain ID"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "0"}, "positive uint256 chain ID"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "-1"}, "positive uint256 chain ID"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "0x10"}, "positive uint256 chain ID"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "invalid"}, "positive uint256 chain ID"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "1"}, "not in the deployment"},
		{[]string{"--qkc.role", "slave", "--qkc.shard", "0"}, "flag provided but not defined: -qkc.shard"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "100003"}, "not in the deployment"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "4294967295"}, "not in the deployment"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "4294967296"}, "not in the deployment"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "18446744073709551616"}, "not in the deployment"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", new(big.Int).Lsh(big.NewInt(1), 256).String()}, "positive uint256 chain ID"},
		{[]string{"--qkc.role", "master", "--syncmode", "full"}, "requires slave role"},
		{[]string{"--qkc.role", "master", "--rpc.gascap", "0"}, "requires slave role"},
		{[]string{"--qkc.role", "master", "--networkid", "100001"}, "must match the positive NetworkID"},
		{[]string{"--qkc.role", "master", "--db.engine", "unknown"}, "invalid Node.DBEngine"},
		{[]string{"--qkc.role", "master", "--port", "-1"}, "invalid port"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			_, err := readQKCFlags(append([]string{"--config", qkcExampleCluster}, test.args...)...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestCheckQKCConfig(t *testing.T) {
	savedDataDirFlag := *utils.DataDirFlag
	t.Cleanup(func() { *utils.DataDirFlag = savedDataDirFlag })
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--qkc.role", "master"}, "master"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "100001"}, "slave, chain ID 100001"},
		{[]string{"--qkc.role", "slave", "--qkc.chain", "100002"}, "slave, chain ID 100002"},
	} {
		var out bytes.Buffer
		datadir := filepath.Join(t.TempDir(), "unused")
		app := newApp()
		app.Writer = &out
		args := append([]string{"qkc", "checkconfig", "--config", qkcExampleCluster, "--datadir", datadir, "--ipcdisable"}, test.args...)
		if err := app.Run(args); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(datadir); !os.IsNotExist(err) {
			t.Fatalf("configuration check created the data directory: %v", err)
		}
		if want := "QuarkChain configuration is valid (" + test.want + ", network ID 2)\n"; out.String() != want {
			t.Fatalf("unexpected validation output: %s", &out)
		}
	}
}
