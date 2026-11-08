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

package genesis

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
)

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
			c := exampleInputs(t)
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
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
}

func TestShardGenesisGasLimit(t *testing.T) {
	for _, test := range []struct {
		name     string
		gasLimit uint64
		wantErr  bool
	}{
		{"below deposit budget", 17999, true},
		{"exact deposit budget", 18000, false},
		{"odd gas limit", 18001, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := exampleInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			g.GasLimit, g.ExtraData = test.gasLimit, nil
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			bundle, err := Load(c)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "gasLimit must be between") {
					t.Fatalf("have %v, want invalid gasLimit", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			extra, err := bundle.Shards[0].Block.Header().QKCExtra()
			if err != nil {
				t.Fatal(err)
			}
			if bundle.Shards[0].Block.GasLimit() != test.gasLimit || extra.XShardGasLimit != params.XShardSendGas {
				t.Fatalf("unexpected genesis gas limits: block %d, cross-shard %d", bundle.Shards[0].Block.GasLimit(), extra.XShardGasLimit)
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
			c := exampleInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			e := &types.QKCExtra{ChainID: new(big.Int).Set(g.Config.ChainID), Cursor: types.XShardCursor{RootBlockHeight: 1}, XShardGasLimit: g.GasLimit / 2}
			test.change(e)
			g.ExtraData = e.Encode()
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), "extraData conflicts") {
				t.Fatalf("invalid bootstrap extra accepted: %v", err)
			}
		})
	}
}

func TestShardGenesisConfigMatching(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*params.ChainConfig)
		want   string
	}{
		{"chain ID", func(c *params.ChainConfig) { c.ChainID.SetInt64(100002) }, "does not match deployed"},
		{"future fork", func(c *params.ChainConfig) { c.LondonBlock.SetInt64(10) }, "LondonBlock"},
		{"omitted fork", func(c *params.ChainConfig) { c.LondonBlock = nil }, "LondonBlock"},
		{"consensus", func(c *params.ChainConfig) { c.QuarkChain.ConsensusType = params.ConsensusSimulate }, ""},
		{"PoSW", func(c *params.ChainConfig) { c.QuarkChain.PoSW.CoinbaseAmount.SetInt64(1) }, ""},
		{"topology", func(c *params.ChainConfig) {
			c.QuarkChain.ChainIDs[0], c.QuarkChain.ChainIDs[1] = c.QuarkChain.ChainIDs[1], c.QuarkChain.ChainIDs[0]
		}, "must match in order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := exampleInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			test.change(g.Config)
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			bundle, err := Load(c)
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("have %v, want %q", err, test.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("independent shard consensus rules rejected: %v", err)
			}
			want, _ := json.Marshal(g.Config)
			got, _ := json.Marshal(bundle.Shards[0].Genesis.Config)
			if !bytes.Equal(got, want) {
				t.Fatal("genesis configuration was replaced by another source")
			}
		})
	}
}

func TestShardGenesisAllocationMerge(t *testing.T) {
	c := exampleInputs(t)
	updateRootConfig(t, c.Master.GenesisFile, func(c *config.RootConfig) { c.PoSW.StakingEnabled = false })
	g := readShard(t, c.Slaves[0].GenesisFile)
	g.Config.QuarkChain.PoSW.StakingEnabled = false
	address := params.ShardStakingContractAddress
	storage := map[common.Hash]common.Hash{{1}: {2}}
	g.Alloc[address] = types.Account{Balance: big.NewInt(123), Nonce: 9, Storage: storage}
	writeJSON(t, c.Slaves[0].GenesisFile, g)
	bundle, err := Load(c)
	if err != nil {
		t.Fatal(err)
	}
	account := bundle.Shards[0].Genesis.Alloc[address]
	if account.Balance.Int64() != 123 || account.Nonce != 9 || account.Storage[common.Hash{1}] != (common.Hash{2}) || !bytes.Equal(account.Code, params.PoSWStakingRuntimeCode) {
		t.Fatal("canonical code merge replaced caller allocation state")
	}
	if len(bundle.Shards[0].Genesis.Alloc[params.RootStakingContractAddress].Code) == 0 {
		t.Fatal("disabled staking removed a required genesis contract")
	}
	// Root staking code on another chain is rejected rather than silently removed.
	other := readShard(t, c.Slaves[1].GenesisFile)
	other.Alloc[params.RootStakingContractAddress] = types.Account{Balance: new(big.Int), Code: bytes.Clone(params.PoSWStakingRuntimeCode)}
	writeJSON(t, c.Slaves[1].GenesisFile, other)
	if _, err := Load(c); err == nil || !strings.Contains(err.Error(), "only be deployed") {
		t.Fatalf("root staking deployed on another shard: %v", err)
	}
	if _, err := LoadShard(c.Slaves[1].GenesisFile); err == nil || !strings.Contains(err.Error(), "only be deployed") {
		t.Fatalf("local genesis loader accepted root staking on another shard: %v", err)
	}
}

func TestShardGenesisActiveSystemContracts(t *testing.T) {
	c := exampleInputs(t)
	spec := readShard(t, c.Slaves[0].GenesisFile)
	cfg := spec.Config
	cfg.ShanghaiTime, cfg.CancunTime, cfg.PragueTime, cfg.OsakaTime, cfg.AmsterdamTime = new(uint64), new(uint64), new(uint64), new(uint64), new(uint64)
	cfg.BlobScheduleConfig = &params.BlobScheduleConfig{Cancun: params.DefaultCancunBlobConfig, Prague: params.DefaultPragueBlobConfig}
	writeJSON(t, c.Slaves[0].GenesisFile, spec)
	bundle, err := Load(c)
	if err != nil {
		t.Fatal(err)
	}
	g, h := bundle.Shards[0].Genesis, bundle.Shards[0].Block.Header()
	for _, address := range cfg.ActiveSystemContracts(g.Timestamp) {
		if address != (common.Address{}) && len(g.Alloc[address].Code) == 0 {
			t.Fatalf("missing active system contract %s", address)
		}
	}
	if h.WithdrawalsHash == nil || h.RequestsHash == nil || h.ParentBeaconRoot == nil || h.BlobGasUsed == nil || *h.BlobGasUsed != 0 || h.SlotNumber == nil || *h.SlotNumber != 0 {
		t.Fatal("fork-dependent empty QKC header fields were not derived")
	}
	canonical := bytes.Clone(params.BeaconRootsCode)
	g.Alloc[params.BeaconRootsAddress].Code[0] ^= 1
	if !bytes.Equal(params.BeaconRootsCode, canonical) {
		t.Fatal("generated code aliases native system contract definitions")
	}
	if len(bundle.Shards[1].Genesis.Alloc[params.BeaconRootsAddress].Code) != 0 {
		t.Fatal("future system contracts injected before their fork")
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
			c := exampleInputs(t)
			writeFile(t, c.Slaves[0].GenesisFile, []byte(input))
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), c.Slaves[0].GenesisFile) {
				t.Fatalf("invalid JSON accepted or error lacks file context: %v", err)
			}
		})
	}
}

func TestShardGenesisBeforeLondon(t *testing.T) {
	c := exampleInputs(t)
	for i, slave := range c.Slaves {
		g := readShard(t, slave.GenesisFile)
		g.Config.LondonBlock = big.NewInt(10)
		if i == 0 {
			g.Config.ShanghaiTime, g.Config.CancunTime = new(uint64), new(uint64)
			g.Config.BlobScheduleConfig = &params.BlobScheduleConfig{Cancun: params.DefaultCancunBlobConfig}
		}
		writeJSON(t, slave.GenesisFile, g)
	}
	bundle, err := Load(c)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Shards[0].Block.BaseFee() != nil || len(bundle.Shards[0].Genesis.Alloc[params.BeaconRootsAddress].Code) != 0 {
		t.Fatal("future London rules changed genesis")
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
			c := exampleInputs(t)
			var input map[string]json.RawMessage
			if err := decodeJSON(c.Slaves[0].GenesisFile, &input); err != nil {
				t.Fatal(err)
			}
			delete(input, "mixHash")
			if test.mixHash != nil {
				input["mixHash"] = test.mixHash
			}
			writeJSON(t, c.Slaves[0].GenesisFile, input)
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), "mixHash is required and must be nonzero") || !strings.Contains(err.Error(), c.Slaves[0].GenesisFile) {
				t.Fatalf("missing or zero QKC difficulty accepted or error lacks file context: %v", err)
			}
		})
	}
}

func TestShardGenesisPoWDifficulty(t *testing.T) {
	c := exampleInputs(t)
	g := readShard(t, c.Slaves[0].GenesisFile)
	g.Config.QuarkChain.PoSW.MinimumDifficulty.SetInt64(2)
	g.Mixhash = common.BigToHash(big.NewInt(2))
	writeJSON(t, c.Slaves[0].GenesisFile, g)
	bundle, err := Load(c)
	if err != nil || bundle.Shards[0].Block.Header().QKCDifficulty().Int64() != 2 {
		t.Fatalf("explicit difficulty equal to PoSW minimum was not preserved: %v", err)
	}
	g = bundle.Shards[0].Genesis
	g.Mixhash = common.BigToHash(big.NewInt(3))
	extra, _ := types.DecodeQKCExtra(g.ExtraData)
	extra.Nonce = types.EncodeNonce(7)
	extra.MixHash = common.Hash{9}
	g.ExtraData = extra.Encode()
	writeJSON(t, c.Slaves[0].GenesisFile, g)
	bundle, err = Load(c)
	if err != nil {
		t.Fatal(err)
	}
	header := bundle.Shards[0].Block.Header()
	extra, _ = header.QKCExtra()
	if header.QKCDifficulty().Int64() != 3 || header.Nonce != (types.BlockNonce{}) || extra.Nonce != types.EncodeNonce(7) || extra.MixHash != (common.Hash{9}) {
		t.Fatal("explicit QKC difficulty or seal was not preserved")
	}
	g.Mixhash = common.BigToHash(big.NewInt(1))
	writeJSON(t, c.Slaves[0].GenesisFile, g)
	if _, err := Load(c); err == nil || !strings.Contains(err.Error(), "below MinimumDifficulty") {
		t.Fatalf("difficulty below PoSW minimum accepted: %v", err)
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
			c := exampleInputs(t)
			g := readShard(t, c.Slaves[0].GenesisFile)
			test.change(g.Config)
			writeJSON(t, c.Slaves[0].GenesisFile, g)
			if _, err := Load(c); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("have %v, want %q", err, test.want)
			}
		})
	}
}
