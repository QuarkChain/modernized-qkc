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
	"context"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"github.com/urfave/cli/v2"
)

func TestM1XShardTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	control := &m1RootAPI{ctx: ctx}
	// Start source shard A (100001) and destination shard B (100002) on a shared R0.
	var backends []*eth.Ethereum
	var clients []*ethclient.Client
	var endpoints []string
	for _, id := range []int{100001, 100002} {
		stack, backend, err := startM1Shard(m1TestConfig(t, id), rpc.API{Namespace: "m1", Service: control})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stack.Close() })
		client, err := ethclient.DialContext(ctx, stack.HTTPEndpoint())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		backends = append(backends, backend)
		clients = append(clients, client)
		endpoints = append(endpoints, stack.HTTPEndpoint())
		t.Logf("Started shard %d: rpc=%s", id, stack.HTTPEndpoint())
	}
	var earlyOutput bytes.Buffer
	earlyApp := &cli.App{Commands: []*cli.Command{m1Command}, Writer: &earlyOutput}
	if err := earlyApp.RunContext(ctx, []string{"qkc", "m1", "root", "--rpc", endpoints[0]}); err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("root command before R0 initialization: %v", err)
	}
	r0, err := initM1Shards(backends)
	if err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	control.backends, control.root = backends, r0
	control.mu.Unlock()
	advanceRoot := func(endpoint string) *qkctypes.RootBlock {
		t.Helper()
		var output bytes.Buffer
		app := &cli.App{Commands: []*cli.Command{m1Command}, Writer: &output}
		if err := app.RunContext(ctx, []string{"qkc", "m1", "root", "--rpc", endpoint}); err != nil {
			t.Fatal(err)
		}
		root := qkcrawdb.ReadRootBlock(backends[0].ChainDb(), backends[0].Shard().RootTip())
		if root == nil || !strings.Contains(output.String(), root.Hash().Hex()) {
			t.Fatalf("root command did not report the stored root: %s", &output)
		}
		return root
	}
	if idle := advanceRoot(endpoints[0]); idle.Hash() != r0.Hash() {
		t.Fatal("idle command changed R0")
	}
	t.Logf("Initialized shared R0 %s; idle driver kept the root unchanged", r0.Hash().TerminalString())
	a, b := backends[0], backends[1]
	balance := func(client *ethclient.Client, address common.Address) *big.Int {
		t.Helper()
		amount, err := client.BalanceAt(ctx, address, nil)
		if err != nil {
			t.Fatal(err)
		}
		return amount
	}
	key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient, coinbase := common.Address{19: 0xbe}, common.Address{19: 0xaa}
	beforeA, beforeB := balance(clients[0], sender), balance(clients[1], recipient)
	beforeCoinbaseB := balance(clients[1], coinbase)
	t.Logf("Balances before transfer: A sender=%s balance=%s wei, B recipient=%s balance=%s wei", sender, beforeA, recipient, beforeB)
	// The outbox call funds both the transferred value and prepaid destination gas.
	outboxABI, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"send","stateMutability":"payable","outputs":[],"inputs":[
		{"name":"dstShard","type":"uint256"},{"name":"to","type":"address"},{"name":"value","type":"uint256"},
		{"name":"gasLimit","type":"uint64"},{"name":"isMove","type":"bool"},{"name":"data","type":"bytes"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	value, gasPrice := big.NewInt(12345), big.NewInt(2*params.InitialBaseFee)
	data, err := outboxABI.Pack("send", b.BlockChain().Config().ChainID, recipient, value, params.XShardSendGas, true, []byte{})
	if err != nil {
		t.Fatal(err)
	}
	callValue := new(big.Int).Mul(new(big.Int).SetUint64(params.XShardSendGas), gasPrice)
	callValue.Add(callValue, value)
	tx := types.MustSignNewTx(key, types.LatestSigner(a.BlockChain().Config()), &types.LegacyTx{
		To: &params.XShardOutboxAddress, Value: callValue, Gas: 120000, GasPrice: gasPrice, Data: data,
	})
	t.Logf("Sending A -> B outbox transaction %s: value=%s wei, callValue=%s wei, destinationGas=%d, gasPrice=%s wei", tx.Hash().TerminalString(), value, callValue, params.XShardSendGas, gasPrice)
	if err := clients[0].SendTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := a.TxPool().Sync(); err != nil {
		t.Fatal(err)
	}
	// Mine A1 through the CLI/RPC path, then match its outbox log to the deposit.
	var output bytes.Buffer
	app := &cli.App{Commands: []*cli.Command{mineCommand}, Writer: &output}
	t.Logf("Mining A1 via RPC: coinbase=%s", coinbase)
	if err := app.RunContext(ctx, []string{"qkc", "mine", "--rpc", endpoints[0], "--coinbase", coinbase.Hex()}); err != nil {
		t.Fatalf("mine A: %v\n%s", err, &output)
	}
	t.Logf("A1 mining client completed:\n%s", &output)
	a1 := a.BlockChain().GetBlockByNumber(1)
	if a1 == nil || len(a1.Transactions()) != 1 || a1.Transactions()[0].Hash() != tx.Hash() {
		t.Fatal("A1 does not contain the sending transaction")
	}
	receipt, err := clients[0].TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful || len(receipt.Logs) != 1 {
		t.Fatalf("outbox transaction: status %d, logs %d", receipt.Status, len(receipt.Logs))
	}
	lists, err := shard.XShardDeposits(a.BlockChain(), a1.Hash())
	if err != nil {
		t.Fatal(err)
	}
	deposits := lists[b.BlockChain().Config().ChainID.String()]
	wantDeposit := &types.DepositTx{
		Version:         types.DepositTxVersion,
		SourceBlockHash: a1.Hash(),
		SourceLogIndex:  uint64(receipt.Logs[0].Index),
		From:            sender,
		To:              &recipient,
		Value:           uint256.MustFromBig(value),
		GasPrice:        uint256.MustFromBig(gasPrice),
		Gas:             params.XShardSendGas,
		IsMove:          true,
		Data:            []byte{},
	}
	if len(deposits) != 1 {
		t.Fatalf("A1 produced %d deposits, want 1", len(deposits))
	}
	if !reflect.DeepEqual(deposits[0], wantDeposit) {
		t.Fatalf("A1 deposit: got %+v, want %+v", deposits[0], wantDeposit)
	}
	t.Logf("Verified A1 %s: sourceGasUsed=%d, deposit to shard %s at log index %d, value=%s wei", a1.Hash().TerminalString(), receipt.GasUsed, b.BlockChain().Config().ChainID, deposits[0].SourceLogIndex, deposits[0].Value)
	// Prime work on R0: confirming A1 must invalidate it even while B is at genesis.
	if _, err := b.PoWMiner().GetWork(ctx, coinbase); err != nil {
		t.Fatal(err)
	}
	t.Log("Primed B mining work on R0 before confirming A1")
	// Deliver A1's deposit and confirm A1 in R1; B must wait for B1 to credit it.
	r1 := advanceRoot(endpoints[0])
	if r1.Number() != 1 || r1.ParentHash() != r0.Hash() || len(r1.ShardBlockHeaders()) != 1 || r1.ShardBlockHeaders()[0].Hash() != a1.Hash() {
		t.Fatal("R1 does not confirm A1 on R0")
	}
	for _, backend := range backends {
		if backend.Shard().RootTip() != r1.Hash() {
			t.Fatal("driver did not distribute R1 to both shards")
		}
	}
	if balance(clients[1], recipient).Cmp(beforeB) != 0 {
		t.Fatal("deposit applied before B mined")
	}
	t.Logf("Distributed R1 %s to both shards: A1 confirmed, B recipient balance still %s wei", r1.Hash().TerminalString(), beforeB)
	// Check the driver's delivery before a retry can overwrite an incorrect list.
	received := qkcrawdb.ReadXShardDeposits(b.ChainDb(), a1.Hash())
	if len(received) != 1 {
		t.Fatalf("B received %d deposits from A1, want 1", len(received))
	}
	if !reflect.DeepEqual(received[0], wantDeposit) {
		t.Fatalf("B received deposit: got %+v, want %+v", received[0], wantDeposit)
	}
	// Repeated delivery, both before and after B1, must not mint the value again.
	if err := b.Shard().AddXshardTxList(a1.Hash(), deposits); err != nil {
		t.Fatal(err)
	}
	t.Log("Repeated A1 deposit delivery before B1")
	// B1 consumes the deposit; B2 and B3 must stay empty across replays and R2.
	root := r1
	bConfig := b.BlockChain().Config().QuarkChain
	destinationFee := new(big.Int).Mul(new(big.Int).SetUint64(params.XShardSendGas), gasPrice)
	wantCoinbaseDelta := bConfig.AfterTax(destinationFee)
	for height := uint64(1); height <= 3; height++ {
		work, err := b.PoWMiner().GetWork(ctx, coinbase)
		if err != nil {
			t.Fatal(err)
		}
		// Shard B's configured POW_SIMULATE validates the block without Ethash search.
		t.Logf("Mining B%d on R%d: work=%s", height, root.Number(), work.SealHash.TerminalString())
		if err := b.PoWMiner().SubmitWork(work.SealHash, types.BlockNonce{}, common.Hash{}); err != nil {
			t.Fatal(err)
		}
		block := b.BlockChain().GetBlockByNumber(height)
		if block == nil {
			t.Fatalf("missing B%d", height)
		}
		extra, err := block.Header().QKCExtra()
		if err != nil {
			t.Fatal(err)
		}
		if extra.PrevRootBlock != root.Hash() || extra.Cursor != (types.XShardCursor{RootBlockHeight: root.NumberU64() + 1}) {
			t.Fatalf("B%d root/cursor mismatch: %+v", height, extra)
		}
		if height == 1 {
			if len(block.Transactions()) != 1 || block.Transactions()[0].Hash() != types.NewTx(wantDeposit).Hash() || extra.XShardGasUsed != params.XShardSendGas {
				t.Fatal("B1 does not contain exactly the expected deposit")
			}
			// Replay the deposit and root after consumption to check idempotent delivery.
			if err := b.Shard().AddXshardTxList(a1.Hash(), deposits); err != nil {
				t.Fatal(err)
			}
			for _, backend := range backends {
				if switched, err := backend.Shard().AddRootBlock(r1); err != nil || switched {
					t.Fatalf("repeated R1: switched %t, error %v", switched, err)
				}
			}
			t.Log("Repeated A1 deposit and R1 delivery after B1; root delivery caused no switch")
		} else if len(block.Transactions()) != 0 || extra.XShardGasUsed != 0 {
			t.Fatalf("B%d replayed the deposit", height)
		}
		delta := new(big.Int).Sub(balance(clients[1], recipient), beforeB)
		if delta.Cmp(value) != 0 {
			t.Fatalf("B%d recipient delta %s, want %s", height, delta, value)
		}
		// Destination gas pays the miner once, independently of block subsidies.
		wantCoinbaseDelta.Add(wantCoinbaseDelta, bConfig.AfterTax(bConfig.PoSW.BlockRewardAt(block.Number())))
		coinbaseDelta := new(big.Int).Sub(balance(clients[1], coinbase), beforeCoinbaseB)
		if coinbaseDelta.Cmp(wantCoinbaseDelta) != 0 {
			t.Fatalf("B%d coinbase delta %s, want %s", height, coinbaseDelta, wantCoinbaseDelta)
		}
		t.Logf("Verified B%d %s: root R%d, cursor %+v, transactions %d, xshardGasUsed %d, recipient delta %s wei", height, block.Hash().TerminalString(), root.Number(), extra.Cursor, len(block.Transactions()), extra.XShardGasUsed, delta)
		if height == 2 {
			// Confirm B1 and B2 too: their empty lists must reach A before R2.
			root = advanceRoot(endpoints[1])
			if root.Number() != 2 || root.ParentHash() != r1.Hash() || len(root.ShardBlockHeaders()) != 2 {
				t.Fatal("R2 does not confirm B1 and B2")
			}
			for _, backend := range backends {
				if backend.Shard().RootTip() != root.Hash() {
					t.Fatal("driver did not distribute R2 to both shards")
				}
			}
			for n := uint64(1); n <= 2; n++ {
				hash := b.BlockChain().GetBlockByNumber(n).Hash()
				if got := root.ShardBlockHeaders()[n-1].Hash(); got != hash {
					t.Fatalf("R2 header %d is %s, want B%d %s", n-1, got, n, hash)
				}
				if list := qkcrawdb.ReadXShardDeposits(a.ChainDb(), hash); list == nil || len(list) != 0 {
					t.Fatalf("A did not receive B%d's empty deposit list", n)
				}
			}
			if idle := advanceRoot(endpoints[0]); idle.Hash() != root.Hash() {
				t.Fatal("idle command changed R2")
			}
			t.Logf("Verified R2 %s: confirmed B1 and B2, delivered both empty deposit lists to A", root.Hash().TerminalString())
		}
	}
	// The sender pays the value plus source and destination gas exactly once.
	spent := new(big.Int).Sub(beforeA, balance(clients[0], sender))
	expected := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed+params.XShardSendGas), receipt.EffectiveGasPrice)
	expected.Add(expected, value)
	if spent.Cmp(expected) != 0 {
		t.Fatalf("A sender spent %s, want %s", spent, expected)
	}
	t.Logf("A sender debit %s: value %s, source gas %d, destination gas %d, effective gas price %s", spent, value, receipt.GasUsed, params.XShardSendGas, receipt.EffectiveGasPrice)
}
