// Copyright 2026 The go-ethereum Authors
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
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

func m1TestConfig(t *testing.T, chainID int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", chainID))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	text := string(data)
	text = strings.Replace(text, "[Eth]", "[Eth]\nDatabaseCache = 16\nTrieCleanCache = 16\nTrieDirtyCache = 16\nSnapshotCache = 0", 1)
	text = strings.Replace(text, fmt.Sprintf("DataDir = %q", fmt.Sprintf("./qkc-data/shard-%d", chainID)), fmt.Sprintf("DataDir = %q", dir), 1)
	text = strings.Replace(text, fmt.Sprintf("IPCPath = %q", fmt.Sprintf("qkc-shard-%d.ipc", chainID)), `IPCPath = ""`, 1)
	text = strings.Replace(text, fmt.Sprintf("HTTPPort = %d", 18545+chainID-100001), "HTTPPort = 0", 1)
	path := filepath.Join(dir, "shard.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMine(t *testing.T) {
	path := m1TestConfig(t, 100001)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise nonce search at a nontrivial difficulty without changing devnet genesis.
	data = []byte(strings.Replace(string(data),
		fmt.Sprintf("Mixhash = %q", common.BigToHash(common.Big1).Hex()),
		fmt.Sprintf("Mixhash = %q", common.BigToHash(big.NewInt(1024)).Hex()), 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Node node.Config
		Eth  ethconfig.Config
	}
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	qkcConfig := config.Eth.Genesis.Config.QuarkChain
	stack, backend, err := startM1Shard(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	root, err := initM1Shards([]*eth.Ethereum{backend})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Loaded POW_ETHASH fixture: chain=%s genesisDifficulty=%s R0=%s", config.Eth.Genesis.Config.ChainID, config.Eth.Genesis.Mixhash.Big(), root.Hash().TerminalString())
	t.Logf("Started shard node and initialized R0: rpc=%s", stack.HTTPEndpoint())
	coinbase := common.Address{0xaa}
	client, err := rpc.DialHTTP(stack.HTTPEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var balanceBefore hexutil.Big
	if err := client.Call(&balanceBefore, "eth_getBalance", coinbase, "0x0"); err != nil {
		t.Fatal(err)
	}
	t.Logf("Coinbase before mining: address=%s balance=%s wei CoinbaseAmount=%s wei rewardTaxRate=%s", coinbase, balanceBefore.ToInt(), qkcConfig.PoSW.CoinbaseAmount, qkcConfig.RewardTaxRate)
	var output bytes.Buffer
	app := &cli.App{Commands: []*cli.Command{mineCommand}, Writer: &output}
	t.Logf("Starting CPU mining client: coinbase=%s", coinbase)
	if err := app.Run([]string{"qkc", "mine", "--rpc", stack.HTTPEndpoint(), "--coinbase", coinbase.Hex()}); err != nil {
		t.Fatalf("mine: %v\n%s", err, &output)
	}
	t.Logf("Mining client completed:\n%s", &output)
	var head *types.Header
	if err := client.Call(&head, "eth_getBlockByNumber", "latest", false); err != nil {
		t.Fatal(err)
	}
	if head == nil || head.Number.Uint64() != 1 || head.Coinbase != coinbase || head.QKCDifficulty().Cmp(common.Big1) <= 0 {
		t.Fatalf("client did not mine the expected block: %+v", head)
	}
	t.Logf("Verified imported block: number=%s hash=%s coinbase=%s difficulty=%s", head.Number, head.Hash().TerminalString(), head.Coinbase, head.QKCDifficulty())
	extra, err := head.QKCExtra()
	if err != nil || extra.MixHash == (common.Hash{}) {
		t.Fatalf("missing Ethash seal: %v", err)
	}
	t.Logf("Confirmed imported seal: nonce=%d mixHash=%s", extra.Nonce.Uint64(), extra.MixHash.TerminalString())
	// With no transactions or fees, the balance increase must equal the subsidy
	// after epoch decay and reward tax, each rounded down separately.
	if head.TxHash != types.EmptyTxsHash || head.GasUsed != 0 {
		t.Fatal("reward check requires an empty block without transaction fees")
	}
	var balanceAfter hexutil.Big
	if err := client.Call(&balanceAfter, "eth_getBalance", coinbase, hexutil.EncodeBig(head.Number)); err != nil {
		t.Fatal(err)
	}
	subsidy := qkcConfig.PoSW.BlockRewardAt(head.Number)
	wantReward := qkcConfig.AfterTax(subsidy)
	credited := new(big.Int).Sub(balanceAfter.ToInt(), balanceBefore.ToInt())
	if credited.Cmp(wantReward) != 0 {
		t.Fatalf("coinbase reward mismatch: before=%s after=%s credited=%s want=%s wei", balanceBefore.ToInt(), balanceAfter.ToInt(), credited, wantReward)
	}
	t.Logf("Verified coinbase reward: subsidyAfterDecay=%s expected=%s credited=%s balanceAfter=%s (wei)", subsidy, wantReward, credited, balanceAfter.ToInt())
}

func TestMineInvalidCoinbase(t *testing.T) {
	app := &cli.App{Commands: []*cli.Command{mineCommand}}
	err := app.Run([]string{"qkc", "mine", "--coinbase", "invalid"})
	if err == nil || !strings.Contains(err.Error(), "invalid coinbase") {
		t.Fatalf("invalid coinbase accepted: %v", err)
	}
}
