// Copyright 2016 The go-ethereum Authors
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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

var customGenesisTests = []struct {
	genesis string
	query   string
	result  string
}{
	// Genesis file with a mostly-empty chain configuration (ensure missing fields work)
	{
		genesis: `{
			"alloc"      : {},
			"coinbase"   : "0x0000000000000000000000000000000000000000",
			"difficulty" : "0x20000",
			"extraData"  : "",
			"gasLimit"   : "0x2fefd8",
			"nonce"      : "0x0000000000001338",
			"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"timestamp"  : "0x00",
			"config": {
				"terminalTotalDifficulty": 0
			}
		}`,
		query:  "eth.getBlock(0).nonce",
		result: "0x0000000000001338",
	},
	// Genesis file with specific chain configurations
	{
		genesis: `{
			"alloc"      : {},
			"coinbase"   : "0x0000000000000000000000000000000000000000",
			"difficulty" : "0x20000",
			"extraData"  : "",
			"gasLimit"   : "0x2fefd8",
			"nonce"      : "0x0000000000001339",
			"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"timestamp"  : "0x00",
			"config"     : {
				"homesteadBlock"                : 42,
				"daoForkBlock"                  : 141,
				"daoForkSupport"                : true,
				"terminalTotalDifficulty": 0
			}
		}`,
		query:  "eth.getBlock(0).nonce",
		result: "0x0000000000001339",
	},
}

// Tests that initializing Geth with a custom genesis block and chain definitions
// work properly.
func TestCustomGenesis(t *testing.T) {
	t.Parallel()
	for i, tt := range customGenesisTests {
		// Create a temporary data directory to use and inspect later
		datadir := t.TempDir()

		// Initialize the data directory with the custom genesis block
		json := filepath.Join(datadir, "genesis.json")
		if err := os.WriteFile(json, []byte(tt.genesis), 0600); err != nil {
			t.Fatalf("test %d: failed to write genesis file: %v", i, err)
		}
		runGeth(t, "--datadir", datadir, "init", json).WaitExit()

		// Query the custom genesis block
		geth := runGeth(t, "--networkid", "1337", "--syncmode=full", "--cache", "16",
			"--datadir", datadir, "--maxpeers", "0", "--port", "0", "--authrpc.port", "0",
			"--nodiscover", "--nat", "none", "--ipcdisable",
			"--exec", tt.query, "console")
		geth.ExpectRegexp(tt.result)
		geth.ExpectExit()
	}
}

// TestLoadShardGenesisConfig checks validation at the TOML file boundary,
// before a node or database is opened.
func TestLoadShardGenesisConfig(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", "..", "qkc", "config", "devnet", "shard-100001.toml"))
	if err != nil {
		t.Fatal(err)
	}
	inline := string(input)
	extra := regexp.MustCompile(`(?m)^ExtraData = .*`).FindString(inline)
	if extra == "" {
		t.Fatal("example is missing ExtraData")
	}
	for _, test := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{name: "ethash"},
		{name: "simulate", from: "POW_ETHASH", to: "POW_SIMULATE"},
		{name: "omitted balance", from: `Balance = "1000000000000000000000000"`},
		{name: "consensus", from: "POW_ETHASH", to: "NONE", want: "invalid ConsensusType"},
		{name: "number", from: "Nonce = 0", to: "Nonce = 0\nNumber = 1", want: "number, parent hash and gas used must be zero"},
		{name: "nonce", from: "Nonce = 0", to: "Nonce = 1", want: "difficulty and nonce must be zero"},
		{name: "difficulty", from: `Difficulty = "0"`, to: `Difficulty = "1"`, want: "difficulty and nonce must be zero"},
		{name: "missing difficulty", from: `Difficulty = "0"`, want: "difficulty and nonce must be zero"},
		{name: "PoW difficulty", from: `MinimumDifficulty = "1"`, to: `MinimumDifficulty = "2"`, want: "below minimum difficulty"},
		{name: "gas limit", from: "GasLimit = 12000000", to: "GasLimit = 17999", want: "invalid gas limit"},
		{name: "cross-shard gas", from: "GasLimit = 12000000", to: "GasLimit = 13000000", want: "invalid cross-shard gas limits"},
		{name: "extra-data", from: extra, to: "ExtraData = [0]", want: "invalid extra-data"},
		{name: "chain ID", from: `ChainID = "100001"`, to: `ChainID = "100002"`, want: "invalid version or chain ID"},
		{name: "negative balance", from: `Balance = "1000000000000000000000000"`, to: `Balance = "-1"`, want: "balance must be a nonnegative uint256"},
		{name: "large balance", from: `Balance = "1000000000000000000000000"`, to: `Balance = "115792089237316195423570985008687907853269984665640564039457584007913129639936"`, want: "balance must be a nonnegative uint256"},
		{name: "base fee", from: "Nonce = 0", to: "Nonce = 0\nBaseFee = \"-1\"", want: "base fee requires London"},
		{name: "blob gas", from: "Nonce = 0", to: "Nonce = 0\nBlobGasUsed = 0", want: "blob gas fields require Cancun"},
		{name: "slot", from: "Nonce = 0", to: "Nonce = 0\nSlotNumber = 0", want: "slot number requires Amsterdam"},
		{name: "merge", from: `ChainID = "100001"`, to: "ChainID = \"100001\"\nTerminalTotalDifficulty = \"0\"", want: "Ethereum Merge transition"},
		{name: "UBT", from: `ChainID = "100001"`, to: "ChainID = \"100001\"\nEnableUBTAtGenesis = true", want: "does not support UBT"},
		{name: "staking code", from: "Code = [96", to: "Code = [0", want: "unexpected genesis code hash"},
		{name: "database only", from: inline, to: strings.Split(inline, "[Eth.Genesis]")[0]},
		{name: "ethereum", from: inline, to: "[Eth.Genesis]\nDifficulty = \"0\"\n[Eth.Genesis.Config]\nChainID = \"1337\"\nTerminalTotalDifficulty = \"0\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := inline
			if test.from != "" {
				if !strings.Contains(inline, test.from) {
					t.Fatalf("example is missing %q", test.from)
				}
				config = strings.Replace(inline, test.from, test.to, 1)
			}
			file := filepath.Join(t.TempDir(), "shard.toml")
			if err := os.WriteFile(file, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			var cfg gethConfig
			err := loadConfig(file, &cfg)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), file) {
				t.Fatalf("loadConfig: got %v, want %q with the config filename", err, test.want)
			}
		})
	}
}

// TestShardGenesis exercises native TOML initialization and database-only restart.
func TestShardGenesis(t *testing.T) {
	t.Parallel()
	example := filepath.Join("..", "..", "qkc", "config", "devnet", "shard-100001.toml")
	var cfg gethConfig
	if err := loadConfig(example, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Eth.Genesis == nil || cfg.Eth.Genesis.Config == nil || cfg.Eth.Genesis.Config.QuarkChain == nil {
		t.Fatal("example is missing shard genesis configuration")
	}
	contents, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	inline := string(contents)
	withoutGenesis, _, ok := strings.Cut(inline, "[Eth.Genesis]")
	if !ok {
		t.Fatal("example is missing the Genesis table")
	}
	datadir := t.TempDir()
	configFile := filepath.Join(t.TempDir(), "shard.toml")
	query := fmt.Sprintf(`[eth.chainId(), net.version, eth.blockNumber,
		eth.getBlock(0).hash, eth.getBlock(0).gasLimit,
		eth.getCode(%q).length, eth.getCode(%q).length,
		rpc.modules.engine === undefined,
		eth.getBalance("0x71562b71999873DB5b286dF957af199Ec94617F7").eq("1000000000000000000000000"),
		eth.getBalance("0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF").eq("1000000000000000000000000")].join(":")`,
		params.RootStakingContractAddress.Hex(), params.ShardStakingContractAddress.Hex())
	// Pin the devnet genesis, including its funded test accounts.
	want := fmt.Sprintf("0x186a1:2:0:0x60735840571556edc1765b4d0e1b97d269557f0c716dbbf4755298daf24aac22:12000000:%d:%d:true:true:true",
		2+len(params.PoSWStakingRuntimeCode)*2, 2+len(params.PoSWStakingRuntimeCode)*2)
	for _, tc := range []struct {
		name   string
		config string
		err    string
	}{
		{name: "initialize", config: inline},
		{name: "restart", config: inline},
		{name: "different genesis", config: strings.Replace(inline, "Timestamp = 0", "Timestamp = 1", 1), err: "database contains incompatible genesis"},
		{name: "different rules", config: strings.Replace(inline, "MaxBlocksPerRootBlock = 18", "MaxBlocksPerRootBlock = 19", 1), err: "quarkchain config differs from stored config"},
		{name: "different consensus", config: strings.Replace(inline, "POW_ETHASH", "POW_SIMULATE", 1), err: "quarkchain config differs from stored config"},
		{name: "invalid consensus", config: strings.Replace(inline, "POW_ETHASH", "NONE", 1), err: "invalid ConsensusType"},
		{name: "database only", config: withoutGenesis},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(configFile, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			geth := runGeth(t, "--config", configFile, "--datadir", datadir,
				"--cache", "16", "--http.port", "0", "--authrpc.port", "0",
				"--port", "0", "--maxpeers", "0", "--nodiscover", "--discovery.dns", "",
				"--nat", "none", "--ipcdisable", "--exec", query, "console")
			if tc.err != "" {
				geth.ExpectRegexp("Fatal: .*" + regexp.QuoteMeta(tc.err) + ".*\n")
			} else {
				geth.Expect(fmt.Sprintf("%q\n", want))
			}
			geth.ExpectExit()
			if (geth.ExitStatus() != 0) != (tc.err != "") {
				t.Fatalf("exit status %d, expected failure: %t", geth.ExitStatus(), tc.err != "")
			}
			if tc.err == "" {
				logs := geth.StderrText()
				if strings.Contains(logs, "Starting Geth on Ethereum mainnet") {
					t.Fatalf("unexpected shard startup logs:\n%s", logs)
				}
			}
		})
	}
}

// TestCustomBackend that the backend selection and detection (leveldb vs pebble) works properly.
func TestCustomBackend(t *testing.T) {
	t.Parallel()
	// Test pebble, but only on 64-bit platforms
	if strconv.IntSize != 64 {
		t.Skip("Custom backends are only available on 64-bit platform")
	}
	genesis := `{
		"alloc"      : {},
		"coinbase"   : "0x0000000000000000000000000000000000000000",
			"difficulty" : "0x20000",
			"extraData"  : "",
			"gasLimit"   : "0x2fefd8",
			"nonce"      : "0x0000000000001338",
			"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
			"timestamp"  : "0x00",
			"config": {
				"terminalTotalDifficulty": 0
			}
		}`
	type backendTest struct {
		initArgs   []string
		initExpect string
		execArgs   []string
		execExpect string
	}
	testfunc := func(t *testing.T, tt backendTest) error {
		// Create a temporary data directory to use and inspect later
		datadir := t.TempDir()

		// Initialize the data directory with the custom genesis block
		json := filepath.Join(datadir, "genesis.json")
		if err := os.WriteFile(json, []byte(genesis), 0600); err != nil {
			return fmt.Errorf("failed to write genesis file: %v", err)
		}
		{ // Init
			args := append(tt.initArgs, "--datadir", datadir, "init", json)
			geth := runGeth(t, args...)
			geth.ExpectRegexp(tt.initExpect)
			geth.ExpectExit()
		}
		{ // Exec + query
			args := append(tt.execArgs, "--networkid", "1337", "--syncmode=full", "--cache", "16",
				"--datadir", datadir, "--maxpeers", "0", "--port", "0", "--authrpc.port", "0",
				"--nodiscover", "--nat", "none", "--ipcdisable",
				"--exec", "eth.getBlock(0).nonce", "console")
			geth := runGeth(t, args...)
			geth.ExpectRegexp(tt.execExpect)
			geth.ExpectExit()
		}
		return nil
	}
	for i, tt := range []backendTest{
		{ // When not specified, it should default to pebble
			execArgs:   []string{"--db.engine", "pebble"},
			execExpect: "0x0000000000001338",
		},
		{ // Explicit leveldb
			initArgs:   []string{"--db.engine", "leveldb"},
			execArgs:   []string{"--db.engine", "leveldb"},
			execExpect: "0x0000000000001338",
		},
		{ // Explicit leveldb first, then autodiscover
			initArgs:   []string{"--db.engine", "leveldb"},
			execExpect: "0x0000000000001338",
		},
		{ // Explicit pebble
			initArgs:   []string{"--db.engine", "pebble"},
			execArgs:   []string{"--db.engine", "pebble"},
			execExpect: "0x0000000000001338",
		},
		{ // Explicit pebble, then auto-discover
			initArgs:   []string{"--db.engine", "pebble"},
			execExpect: "0x0000000000001338",
		},
		{ // Can't start pebble on top of leveldb
			initArgs:   []string{"--db.engine", "leveldb"},
			execArgs:   []string{"--db.engine", "pebble"},
			execExpect: `Fatal: Failed to register the Ethereum service: db.engine choice was pebble but found pre-existing leveldb database in specified data directory`,
		},
		{ // Can't start leveldb on top of pebble
			initArgs:   []string{"--db.engine", "pebble"},
			execArgs:   []string{"--db.engine", "leveldb"},
			execExpect: `Fatal: Failed to register the Ethereum service: db.engine choice was leveldb but found pre-existing pebble database in specified data directory`,
		},
		{ // Reject invalid backend choice
			initArgs:   []string{"--db.engine", "mssql"},
			initExpect: `Fatal: Invalid choice for db.engine 'mssql', allowed 'leveldb' or 'pebble'`,
			// Since the init fails, this will return the (default) mainnet genesis
			// block nonce
			execExpect: `0x0000000000000042`,
		},
	} {
		if err := testfunc(t, tt); err != nil {
			t.Fatalf("test %d-leveldb: %v", i, err)
		}
	}
}
