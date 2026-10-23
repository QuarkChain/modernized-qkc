// Copyright 2026 The quarkchain Authors
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

package miner

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Header sealing is outside block construction; execute and validate the body normally.
type qkcTestEngine struct{ consensus.Engine }

func (qkcTestEngine) VerifyHeader(consensus.ChainHeaderReader, *types.Header) error { return nil }
func (qkcTestEngine) VerifyHeaders(_ consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	results := make(chan error, len(headers))
	for range headers {
		results <- nil
	}
	return make(chan struct{}), results
}

type miningInbox struct {
	deposits  []*types.DepositTx
	failAfter int
	err       error
}

func (in *miningInbox) Deposits(*types.Header, common.Hash) (core.XShardDeposits, error) {
	return &miningDeposits{inbox: in}, nil
}

type miningDeposits struct {
	inbox    *miningInbox
	consumed int
	eof      bool
}

func (d *miningDeposits) Next() (*types.DepositTx, error) {
	if d.inbox.err != nil && d.consumed == d.inbox.failAfter {
		return nil, d.inbox.err
	}
	if d.consumed == len(d.inbox.deposits) {
		d.eof = true
		return nil, nil
	}
	tx := d.inbox.deposits[d.consumed]
	d.consumed++
	return tx, nil
}

func (d *miningDeposits) Cursor() types.XShardCursor {
	if d.eof {
		return types.XShardCursor{RootBlockHeight: 2}
	}
	return types.XShardCursor{RootBlockHeight: 1, XShardDepositIndex: uint64(d.consumed)}
}

func TestBuildXShardDeposits(t *testing.T) {
	missing := errors.New("missing deposit list")
	for _, amsterdam := range []bool{false, true} {
		fork := "osaka"
		if amsterdam {
			fork = "amsterdam"
		}
		t.Run(fork, func(t *testing.T) {
			for _, tt := range []struct {
				name                       string
				limit                      uint64
				available, included, users int
				eof, noTxs, missingInbox   bool
				failAfter                  int
				iteratorErr, wantErr       error
				edit                       func([]*types.DepositTx)
			}{
				{name: "empty inbox", limit: 100000, eof: true},
				{name: "empty below move gas", limit: 17000, eof: true},
				{name: "first does not fit", limit: 17000, available: 1},
				{name: "odd gas limit", limit: 100001, available: 6, included: 5},
				{name: "exact with backlog", limit: 90000, available: 6, included: 5},
				{name: "exact at EOF", limit: 90000, available: 5, included: 5, eof: true},
				{name: "unused deposit gas goes to users", limit: 100000, available: 2, included: 2, users: 3, eof: true},
				{name: "deposits precede users", limit: 100000, available: 6, included: 5, users: 2},
				{name: "noTxs keeps deposits", limit: 100000, available: 2, included: 2, users: 3, noTxs: true, eof: true},
				{name: "zero price reward", limit: 100000, available: 1, included: 1, eof: true,
					edit: func(ds []*types.DepositTx) { ds[0].GasPrice.Clear() }},
				{name: "declared gas blocks next deposit", limit: 100000, available: 2, included: 1,
					edit: func(ds []*types.DepositTx) { ds[1].Gas = 41001 }},
				{name: "non move rejected", limit: 100000, available: 1, wantErr: core.ErrDepositNotMove,
					edit: func(ds []*types.DepositTx) { ds[0].IsMove = false }},
				{name: "missing initial list", limit: 100000, iteratorErr: missing, wantErr: missing},
				{name: "missing list at exact exhaustion", limit: 90000, available: 5, failAfter: 5, iteratorErr: missing, wantErr: missing},
				{name: "missing inbox", limit: 100000, missingInbox: true},
			} {
				t.Run(tt.name, func(t *testing.T) {
					config := *params.MergedTestChainConfig
					config.QuarkChain = &params.QuarkChainConfig{ShardID: 1, ShardIDs: []uint32{0, 1}}
					if amsterdam {
						config.AmsterdamTime = new(uint64)
					}
					alloc := core.SystemContractAllocs()
					alloc[testBankAddress] = types.Account{Balance: testBankFunds}
					// Deposits to a contract must remain moves, even if its code reverts.
					alloc[testUserAddress] = types.Account{Code: common.FromHex("0x60006000fd"), Balance: new(big.Int)}
					genesis := &core.Genesis{
						Config: &config, GasLimit: tt.limit,
						ExtraData: (&types.QKCExtra{ShardID: 1, Cursor: types.XShardCursor{RootBlockHeight: 1}}).Encode(),
						Alloc:     alloc,
					}
					engine := qkcTestEngine{beacon.New(ethash.NewFaker())}
					chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer chain.Stop()
					pool, err := txpool.New(testTxPoolConfig.PriceLimit, chain, []txpool.SubPool{legacypool.New(testTxPoolConfig, chain)})
					if err != nil {
						t.Fatal(err)
					}
					defer pool.Close()
					inbox := &miningInbox{failAfter: tt.failAfter, err: tt.iteratorErr}
					for i := range tt.available {
						inbox.deposits = append(inbox.deposits, &types.DepositTx{
							Version: types.DepositTxVersion, SourceBlockHash: common.Hash{1}, SourceLogIndex: uint64(i),
							From: testBankAddress, To: &testUserAddress, Value: uint256.NewInt(1),
							Gas: params.XShardSendGas, GasPrice: uint256.NewInt(params.GWei), IsMove: true,
						})
					}
					if tt.edit != nil {
						tt.edit(inbox.deposits)
					}
					if !tt.missingInbox {
						chain.SetXShardInbox(inbox)
					}
					for i := range tt.users {
						tx := types.MustSignNewTx(testBankKey, types.LatestSigner(&config), &types.DynamicFeeTx{
							ChainID: config.ChainID, Nonce: uint64(i), Gas: params.TxGas,
							To: &common.Address{0xee}, Value: big.NewInt(1),
							GasFeeCap: big.NewInt(2 * params.GWei), GasTipCap: big.NewInt(params.GWei),
						})
						if err := pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
							t.Fatal(err)
						}
					}
					mc := DefaultConfig
					mc.GasCeil = tt.limit
					miner := New(NewMockBackend(chain, pool), mc, engine)
					args := &generateParams{
						timestamp: 1, forceTime: true, parentHash: chain.Genesis().Hash(),
						coinbase: common.Address{0xcc}, beaconRoot: new(common.Hash), slotNum: new(uint64),
						qkc: &types.QKCExtra{ShardID: 1, PrevRootBlock: common.Hash{0x77}}, noTxs: tt.noTxs,
					}
					result := miner.generateWork(context.Background(), args, false)
					if amsterdam && tt.limit < 2*params.XShardSendGas {
						// System calls alone exceed the BAL budget. Reject the candidate
						// rather than returning a block that cannot be imported.
						if result.err == nil || !strings.Contains(result.err.Error(), "block access list exceeds size constraint") {
							t.Fatalf("expected access-list size error, got %v", result.err)
						}
						return
					}
					if tt.missingInbox {
						if result.err == nil {
							t.Fatal("built without an inbox")
						}
						return
					}
					if tt.wantErr != nil {
						if !errors.Is(result.err, tt.wantErr) {
							t.Fatalf("error %v, want %v", result.err, tt.wantErr)
						}
						return
					}
					if result.err != nil {
						t.Fatal(result.err)
					}
					extra, err := result.block.Header().QKCExtra()
					if err != nil {
						t.Fatal(err)
					}
					cursor := types.XShardCursor{RootBlockHeight: 1, XShardDepositIndex: uint64(tt.included)}
					if tt.eof {
						cursor = types.XShardCursor{RootBlockHeight: 2}
					}
					if extra.Cursor != cursor || extra.XShardGasLimit != tt.limit/2 || extra.XShardGasUsed != uint64(tt.included)*params.XShardSendGas {
						t.Fatalf("extra %+v, want cursor %+v and %d deposits", extra, cursor, tt.included)
					}
					users := tt.users
					if tt.noTxs {
						users = 0
					}
					txs := result.block.Transactions()
					if len(txs) != tt.included+users {
						t.Fatalf("transactions %d, want %d", len(txs), tt.included+users)
					}
					for i := range tt.included {
						if txs[i].Hash() != types.NewTx(inbox.deposits[i]).Hash() {
							t.Fatalf("deposit %d differs", i)
						}
					}
					if balance := result.stateDB.GetBalance(testUserAddress); !balance.Eq(uint256.NewInt(uint64(tt.included))) {
						t.Fatalf("recipient balance %v, want %d", balance, tt.included)
					}
					if result.stateDB.GetNonce(testBankAddress) != uint64(users) {
						t.Fatal("deposits changed the sender nonce")
					}
					if got := result.stateDB.GetBalance(args.coinbase).ToBig(); got.Cmp(result.fees) != 0 {
						t.Fatalf("coinbase fees %v, reported %v", got, result.fees)
					}
					// Re-execution runs the unchanged validateXShardDeposits rules.
					if _, err := chain.InsertChain([]*types.Block{result.block}); err != nil {
						t.Fatalf("import built block: %v", err)
					}
				})
			}
		})
	}
}
