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
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func genesisInputs(t *testing.T) *ClusterConfig {
	t.Helper()
	c, err := LoadCluster(copyExampleCluster(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func writeFile(t *testing.T, file string, data []byte) {
	t.Helper()
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func writeJSON(t *testing.T, file string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, data)
}
func readShard(t *testing.T, file string) *core.Genesis {
	t.Helper()
	var g core.Genesis
	if err := decodeJSON(file, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

func TestGenesisPreparation(t *testing.T) {
	c := genesisInputs(t)
	g := readShard(t, c.Slaves[0].GenesisFile)
	g.ExtraData = nil
	g.Alloc[params.ShardStakingContractAddress] = types.Account{
		Balance: big.NewInt(42), Nonce: 7, Storage: map[common.Hash]common.Hash{{1}: {2}},
	}
	delete(g.Alloc, params.RootStakingContractAddress)
	writeJSON(t, c.Slaves[0].GenesisFile, g)
	before, err := os.ReadFile(c.Slaves[0].GenesisFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.LoadGenesis(); err != nil {
		t.Fatal(err)
	}
	if c.Master.Root.Timestamp == nil || c.Master.Root.Difficulty == nil {
		t.Fatal("root loader discarded header fields")
	}
	g = c.Slaves[0].Eth.Genesis
	extra, err := types.DecodeQKCExtra(g.ExtraData)
	if err != nil || extra.Cursor.RootBlockHeight != 1 || extra.ChainID.Cmp(g.Config.ChainID) != 0 || extra.XShardGasLimit != g.GasLimit/params.XShardGasLimitDivisor {
		t.Fatalf("loader did not prepare QKC header fields: %v", err)
	}
	account := g.Alloc[params.ShardStakingContractAddress]
	if !bytes.Equal(account.Code, params.PoSWStakingRuntimeCode) || account.Balance.Int64() != 42 || account.Nonce != 7 || account.Storage[common.Hash{1}] != (common.Hash{2}) {
		t.Fatal("loader did not preserve allocation values while filling code")
	}
	if !bytes.Equal(g.Alloc[params.RootStakingContractAddress].Code, params.PoSWStakingRuntimeCode) {
		t.Fatal("loader omitted root staking code on the root reward chain")
	}
	after, err := os.ReadFile(c.Slaves[0].GenesisFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("loader modified the source file: %v", err)
	}
}

func TestSelectedGenesisRejectsRootStakingOnAnotherShard(t *testing.T) {
	c := genesisInputs(t)
	node := &c.Slaves[1]
	g := readShard(t, node.GenesisFile)
	g.Alloc[params.RootStakingContractAddress] = types.Account{Balance: new(big.Int), Code: bytes.Clone(params.PoSWStakingRuntimeCode)}
	writeJSON(t, node.GenesisFile, g)
	if err := node.LoadGenesis(); err == nil || !strings.Contains(err.Error(), "only be deployed") {
		t.Fatalf("local loader accepted root staking on another shard: %v", err)
	}
}

func TestShardGenesisInvalid(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*core.Genesis)
		want   string
	}{
		{"number", func(g *core.Genesis) { g.Number = 1 }, "must be zero"},
		{"parent", func(g *core.Genesis) { g.ParentHash = common.Hash{1} }, "must be zero"},
		{"gas used", func(g *core.Genesis) { g.GasUsed = 1 }, "must be zero"},
		{"nonce", func(g *core.Genesis) { g.Nonce = 1 }, "difficulty and nonce"},
		{"difficulty", func(g *core.Genesis) { g.Difficulty.SetInt64(1) }, "difficulty and nonce"},
		{"negative difficulty", func(g *core.Genesis) { g.Difficulty.SetInt64(-1) }, "difficulty and nonce"},
		{"zero gas", func(g *core.Genesis) { g.GasLimit = 0 }, "gasLimit must be between"},
		{"small gas", func(g *core.Genesis) { g.GasLimit = params.MinGasLimit - 1 }, "gasLimit must be between"},
		{"large gas", func(g *core.Genesis) { g.GasLimit = params.MaxGasLimit + 1 }, "gasLimit must be between"},
		{"negative basefee", func(g *core.Genesis) { g.BaseFee = big.NewInt(-1) }, "baseFeePerGas"},
		{"extra format", func(g *core.Genesis) { g.ExtraData = []byte{1} }, "extra data length"},
		{"negative balance", func(g *core.Genesis) {
			g.Alloc[common.Address{1}] = types.Account{Balance: big.NewInt(-1)}
		}, "balance must be a nonnegative uint256"},
		{"wrong staking code", func(g *core.Genesis) {
			g.Alloc[params.ShardStakingContractAddress] = types.Account{Balance: new(big.Int), Code: []byte{1}}
		}, "conflicting genesis code"},
		{"blob fields before Cancun", func(g *core.Genesis) { g.BlobGasUsed = new(uint64) }, "blob gas fields"},
		{"excess gas before Cancun", func(g *core.Genesis) { g.ExcessBlobGas = new(uint64) }, "blob gas fields"},
		{"slot before Amsterdam", func(g *core.Genesis) { g.SlotNumber = new(uint64) }, "slotNumber"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := genesisInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			test.change(g)
			data, err := json.Marshal(g)
			if err != nil {
				t.Fatal(err)
			}
			// Native codecs accept decimal negatives, although their marshaler
			// emits the invalid negative-hex spelling. Exercise our own checks.
			data = bytes.ReplaceAll(data, []byte(`"-0x1"`), []byte(`"-1"`))
			writeFile(t, c.Slaves[0].GenesisFile, data)
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
}

func TestShardGenesisExtra(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*types.QKCExtra)
	}{
		{"version", func(e *types.QKCExtra) { e.Version = 1 }},
		{"chain", func(e *types.QKCExtra) { e.ChainID.SetInt64(7) }},
		{"root", func(e *types.QKCExtra) { e.PrevRootBlock = common.Hash{1} }},
		{"root cursor", func(e *types.QKCExtra) { e.Cursor.RootBlockHeight = 0 }},
		{"minor cursor", func(e *types.QKCExtra) { e.Cursor.MinorBlockIndex = 1 }},
		{"deposit cursor", func(e *types.QKCExtra) { e.Cursor.XShardDepositIndex = 1 }},
		{"cross gas used", func(e *types.QKCExtra) { e.XShardGasUsed = 1 }},
		{"cross gas limit", func(e *types.QKCExtra) { e.XShardGasLimit-- }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := genesisInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			e := &types.QKCExtra{ChainID: new(big.Int).Set(g.Config.ChainID), Cursor: types.XShardCursor{RootBlockHeight: 1}, XShardGasLimit: g.GasLimit / 2}
			test.change(e)
			g.ExtraData = e.Encode()
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), "extraData conflicts") {
				t.Fatalf("invalid bootstrap extra accepted: %v", err)
			}
		})
	}
}

func TestShardGenesisJSON(t *testing.T) {
	for _, input := range []string{
		`not json`,
		`{"gasLimit":"0x989680","difficulty":"0x0","alloc":{}} {}`,
		`{"gasLimit":"0x989680","alloc":{}}`,
		`{"difficulty":"0x0","alloc":{}}`,
		`{"gasLimit":"0x989680","difficulty":"0x0"}`,
	} {
		t.Run(input, func(t *testing.T) {
			c := genesisInputs(t)
			writeFile(t, c.Slaves[0].GenesisFile, []byte(input))
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), c.Slaves[0].GenesisFile) {
				t.Fatalf("invalid JSON accepted or error lacks file context: %v", err)
			}
		})
	}
}

func TestShardGenesisRequiredMixHash(t *testing.T) {
	for _, test := range []struct {
		name    string
		mixHash json.RawMessage
	}{
		{"missing", nil},
		{"null", json.RawMessage(`null`)},
		{"zero", json.RawMessage(`"0x0000000000000000000000000000000000000000000000000000000000000000"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := genesisInputs(t)
			var input map[string]json.RawMessage
			if err := decodeJSON(c.Slaves[0].GenesisFile, &input); err != nil {
				t.Fatal(err)
			}
			delete(input, "mixHash")
			if test.mixHash != nil {
				input["mixHash"] = test.mixHash
			}
			writeJSON(t, c.Slaves[0].GenesisFile, input)
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), "mixHash is required and must be nonzero") || !strings.Contains(err.Error(), c.Slaves[0].GenesisFile) {
				t.Fatalf("missing or zero QKC difficulty accepted or error lacks file context: %v", err)
			}
		})
	}
}

func TestShardGenesisUnsupportedModes(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*params.ChainConfig)
		want   string
	}{
		{"Clique", func(c *params.ChainConfig) { c.Clique = &params.CliqueConfig{} }, "cannot select Clique"},
		{"Merge", func(c *params.ChainConfig) { c.TerminalTotalDifficulty = new(big.Int) }, "Ethereum Merge"},
		{"UBT", func(c *params.ChainConfig) { c.EnableUBTAtGenesis = true }, "UBT initialization"},
		{"beacon deposit contract", func(c *params.ChainConfig) { c.DepositContractAddress = common.Address{1} }, "DepositContractAddress must be zero"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := genesisInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			test.change(g.Config)
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
}

func TestGenesisLoadCoverage(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ClusterConfig)
		want   string
	}{
		{"root file", func(c *ClusterConfig) { c.Master.GenesisFile = "" }, "Master: GenesisFile"},
		{"shard file", func(c *ClusterConfig) { c.Slaves[0].GenesisFile = "" }, "GenesisFile is required"},
		{"missing chain", func(c *ClusterConfig) { c.Slaves = c.Slaves[:1] }, "every"},
		{"duplicate chain", func(c *ClusterConfig) {
			c.Slaves[1].ChainID = c.Slaves[0].ChainID
			c.Slaves[1].GenesisFile = c.Slaves[0].GenesisFile
		}, "duplicate"},
		{"unknown chain", func(c *ClusterConfig) { c.Slaves[0].ChainID = big.NewInt(7) }, "ChainID"},
		{"nil chain", func(c *ClusterConfig) { c.Slaves[0].ChainID = nil }, "ChainID"},
		{"missing file", func(c *ClusterConfig) { c.Slaves[0].GenesisFile += ".missing" }, ".missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := genesisInputs(t)
			test.change(c)
			if err := c.LoadGenesis(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
}

func rootSeedFile(t *testing.T, input string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "root.json")
	if !strings.Contains(input, `"config":`) {
		cfg, err := json.Marshal(rootSeedConfig())
		if err != nil {
			t.Fatal(err)
		}
		input = strings.Replace(input, "{", `{"config":`+string(cfg)+",", 1)
	}
	if err := os.WriteFile(file, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func rootSeedConfig() RootConfig {
	return RootConfig{
		ConsensusType: params.ConsensusEthash,
		PoSW: &params.PoSWConfig{
			AdjustmentCutoff: 7, AdjustmentFactor: 512, MinimumDifficulty: big.NewInt(2),
			CoinbaseAmount: new(big.Int),
			EpochInterval:  10, DecayFactor: big.NewRat(88, 100),
		},
	}
}

func loadRootInput(file string) error {
	node := DefaultNodeConfig(nil)
	node.GenesisFile = file
	return node.LoadGenesis()
}
func TestLoadRootValidation(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{"missing timestamp", `{"difficulty":"0x2"}`, "timestamp is required"},
		{"null timestamp", `{"timestamp":null,"difficulty":"0x2"}`, "timestamp is required"},
		{"missing difficulty", `{"timestamp":"0x0"}`, "difficulty must be a positive uint256"},
		{"zero difficulty", `{"timestamp":"0x0","difficulty":"0x0"}`, "difficulty must be a positive uint256"},
		{"negative difficulty", `{"timestamp":"0x0","difficulty":"-1"}`, "difficulty must be a positive uint256"},
		{"overflow difficulty", `{"timestamp":"0x0","difficulty":"0x10000000000000000000000000000000000000000000000000000000000000000"}`, "invalid hex or decimal integer"},
		{"below minimum", `{"timestamp":"0x0","difficulty":"0x1"}`, "below MinimumDifficulty"},
		{"derived number", `{"timestamp":"0x0","difficulty":"0x2","number":1}`, "unknown field"},
		{"derived parent", `{"timestamp":"0x0","difficulty":"0x2","parentHash":"0x00"}`, "unknown field"},
		{"derived shard commitment", `{"timestamp":"0x0","difficulty":"0x2","shardHeaderHash":"0x00"}`, "unknown field"},
		{"native allocations", `{"timestamp":"0x0","difficulty":"0x2","alloc":{}}`, "unknown field"},
		{"empty config", `{"timestamp":"0x0","difficulty":"0x2","config":{}}`, "ConsensusType"},
		{"null config", `{"timestamp":"0x0","difficulty":"0x2","config":null}`, "config"},
		{"trailing document", `{"timestamp":"0x0","difficulty":"0x2"} {}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := rootSeedFile(t, test.input)
			err := loadRootInput(file)
			if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want file-aware error containing %q", err, test.want)
			}
		})
	}
	file := rootSeedFile(t, `{"timestamp":0,"difficulty":2}`)
	writeFile(t, file, []byte(`{"timestamp":0,"difficulty":2}`))
	if err := loadRootInput(file); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("missing root config accepted: %v", err)
	}
}
