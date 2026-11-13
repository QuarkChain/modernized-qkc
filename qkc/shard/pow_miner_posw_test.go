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

package shard

import (
	"context"
	"maps"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

type nonceSearchFunc func(context.Context, *MiningWork) (types.BlockNonce, common.Hash, bool)

func (f nonceSearchFunc) Search(ctx context.Context, work *MiningWork) (types.BlockNonce, common.Hash, bool) {
	return f(ctx, work)
}

func TestPoWMinerPoSW(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		const fullDifficulty, reducedDifficulty = 32, 4
		config := params.NewTestQuarkChainConfig(10002, 10002)
		config.QuarkChain.ConsensusType = params.ConsensusEthash
		staking := config.QuarkChain.PoSW
		staking.StakingEnabled, staking.WindowSize, staking.DiffDivider = true, 4, 8
		// The base difficulty stays at 32: it is below AdjustmentFactor (512),
		// so integer difficulty adjustment is zero even across the withdrawal delay.
		staking.MinimumDifficulty = big.NewInt(fullDifficulty)
		staking.StakePerBlock = new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))
		// 20 QKC grants two discounted blocks. WindowSize includes the candidate,
		// so eligibility counts this coinbase's blocks among the preceding three.
		stake := new(big.Int).Mul(big.NewInt(2), staking.StakePerBlock)
		key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		if err != nil {
			t.Fatal(err)
		}
		// Miner A owns the stake; unstaked miner B advances the window.
		coinbase, other := crypto.PubkeyToAddress(key.PublicKey), common.Address{0x42}
		alloc := core.SystemContractAllocs()
		maps.Copy(alloc, core.PoSWStakingContractAllocs(config))
		alloc[coinbase] = types.Account{Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))}
		genesis := &core.Genesis{
			Config: config, Alloc: alloc, GasLimit: 12_000_000,
			Difficulty: new(big.Int), Mixhash: common.BigToHash(staking.MinimumDifficulty),
			ExtraData: (&types.QKCExtra{
				ChainID: config.ChainID, Cursor: types.XShardCursor{RootBlockHeight: 1},
				XShardGasLimit: 12_000_000 / params.XShardGasLimitDivisor,
			}).Encode(),
		}
		db := rawdb.NewMemoryDatabase()
		t.Cleanup(func() { db.Close() })
		// Use real PoSW for both construction and full block import, so seal
		// validation derives the Ethash target from the actual parent state.
		pow, err := posw.New(config, genesis.ToBlock().Header())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pow.Close() })
		chain, err := core.NewBlockChain(db, genesis, pow, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(chain.Stop)
		poolConfig := legacypool.DefaultConfig
		poolConfig.Journal = ""
		pool, err := txpool.New(poolConfig.PriceLimit, chain, []txpool.SubPool{legacypool.New(poolConfig, chain)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pool.Close() })
		minerConfig := miner.DefaultConfig
		minerConfig.GasCeil = genesis.GasLimit
		builder := miner.New(miningBackend{chain, pool}, minerConfig, pow)
		reported := make(chan *types.Header, 1)
		s, err := New(NewShardChain(db, chain, builder), &backendClusterClient{
			broadcast: func(context.Context, *big.Int, common.Hash, []*types.DepositTx) error { return nil },
			report: func(_ context.Context, header *types.Header, _ *uint256.Int) error {
				reported <- header
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
			Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1),
		}, qkctypes.ShardBlockHeaders{s.CurrentBlock()}, nil)
		if err := s.Init(root); err != nil {
			t.Fatal(err)
		}
		t.Logf("Initialized real Ethash PoSW: coinbase=%s WindowSize=%d StakePerBlock=10 QKC DiffDivider=%d", coinbase, staking.WindowSize, staking.DiffDivider)

		fullDifficultyBig := big.NewInt(fullDifficulty)
		search := func(ctx context.Context, work *MiningWork) (types.BlockNonce, common.Hash, bool) {
			// Work can be timestamped at parent.Time+1, ahead of the test clock.
			select {
			case <-time.After(time.Until(time.Unix(int64(work.Time), 0))):
			case <-ctx.Done():
				return types.BlockNonce{}, common.Hash{}, false
			}
			for start := uint64(0); ; {
				nonce, mix, ok := pow.SearchNonce(ctx, work.Number, work.SealHash, work.Difficulty, start)
				if !ok {
					return nonce, mix, false
				}
				if work.Difficulty.Cmp(fullDifficultyBig) == 0 {
					return nonce, mix, true
				}
				// A discounted seal must fail the full target, proving that acceptance
				// depends on staking rather than accidentally satisfying ordinary PoW.
				switch err := pow.EthashVerifier.Verify(work.Number, work.SealHash, nonce.Uint64(), mix, fullDifficultyBig); {
				case err == nil:
					start = nonce.Uint64() + 1
				case err.Error() == "invalid Ethash proof of work":
					return nonce, mix, true
				default:
					t.Errorf("full-difficulty seal verification: %v", err)
					return types.BlockNonce{}, common.Hash{}, false
				}
			}
		}
		mine := func(label string, address common.Address, wantDifficulty int64) *types.Block {
			t.Helper()
			parent := chain.CurrentBlock()
			works := make(chan *MiningWork)
			m := NewPoWMiner(s, PoWMinerConfig{Coinbase: address, Search: nonceSearchFunc(func(ctx context.Context, work *MiningWork) (types.BlockNonce, common.Hash, bool) {
				// The test receives once, releasing exactly one search. The next search
				// blocks here until StopLocalMining cancels it, keeping stake changes
				// and assertions between blocks while exercising the real mining loop.
				select {
				case works <- work:
				case <-ctx.Done():
					return types.BlockNonce{}, common.Hash{}, false
				}
				return search(ctx, work)
			})})
			t.Cleanup(m.StopLocalMining)
			if err := m.StartLocalMining(); err != nil {
				t.Fatal(err)
			}
			var work *MiningWork
			select {
			case work = <-works:
			case <-time.After(time.Minute):
				t.Fatal("timed out waiting for local mining work")
			}
			if work.Difficulty.Cmp(big.NewInt(wantDifficulty)) != 0 {
				t.Fatalf("%s: work difficulty %s, want %d", label, work.Difficulty, wantDifficulty)
			}
			var header *types.Header
			select {
			case header = <-reported:
			case <-time.After(time.Minute):
				t.Fatalf("%s: timed out waiting for a mined block", label)
			}
			m.StopLocalMining()
			if header.ParentHash != parent.Hash() || header.Number.Uint64() != parent.Number.Uint64()+1 || header.Coinbase != address || chain.CurrentBlock().Hash() != header.Hash() {
				t.Fatalf("%s: mined block does not extend the canonical head", label)
			}
			// Headers commit the base difficulty; only the work's seal target
			// receives the staking discount.
			if header.QKCDifficulty().Int64() != fullDifficulty {
				t.Fatalf("%s: header base difficulty %s, want %d", label, header.QKCDifficulty(), fullDifficulty)
			}
			if err := pow.VerifyHeader(chain, header); err != nil {
				t.Fatalf("%s: header verification: %v", label, err)
			}
			if err := pow.VerifySeal(chain, header); err != nil {
				t.Fatalf("%s: seal verification: %v", label, err)
			}
			// Log the parent-state stake and preceding WindowSize-1 ancestors used
			// for eligibility, even if this block itself changes the locked stake.
			locked, err := chain.GetShardLockedStakes(parent, address)
			if err != nil {
				t.Fatal(err)
			}
			var mined uint64
			for ancestor, i := parent, uint64(0); i < staking.WindowSize-1; i++ {
				if ancestor.Coinbase == address {
					mined++
				}
				if ancestor.Number.Sign() == 0 {
					break
				}
				ancestor = chain.GetHeader(ancestor.ParentHash, ancestor.Number.Uint64()-1)
			}
			extra, _ := header.QKCExtra()
			t.Logf("%s: block=#%d coinbase=%s lockedStake=%s QKC previousBlocksInWindow=%d baseDifficulty=%s requiredDifficulty=%s nonce=%d validation=accepted", label, header.Number, address, new(big.Int).Div(locked.ToBig(), big.NewInt(params.Ether)), mined, header.QKCDifficulty(), work.Difficulty, extra.Nonce.Uint64())
			return chain.GetBlockByHash(header.Hash())
		}
		var nonce uint64
		send := func(value *big.Int, data []byte) *types.Transaction {
			t.Helper()
			tx := types.MustSignNewTx(key, types.LatestSigner(config), &types.LegacyTx{
				Nonce: nonce, To: &params.ShardStakingContractAddress, Value: value,
				Gas: 200_000, GasPrice: big.NewInt(2 * params.GWei), Data: data,
			})
			if err := pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
				t.Fatal(err)
			}
			nonce++
			return tx
		}
		checkTransaction := func(block *types.Block, tx *types.Transaction) {
			t.Helper()
			for _, receipt := range chain.GetReceiptsByHash(block.Hash()) {
				if receipt.TxHash == tx.Hash() {
					if receipt.Status != types.ReceiptStatusSuccessful {
						t.Fatalf("staking transaction %s reverted", tx.Hash())
					}
					return
				}
			}
			t.Fatalf("staking transaction %s missing from mined block", tx.Hash())
		}
		checkStake := func(wantLocked, wantBalance *big.Int) {
			t.Helper()
			locked, err := chain.GetShardLockedStakes(chain.CurrentBlock(), coinbase)
			if err != nil {
				t.Fatal(err)
			}
			state, err := chain.State()
			if err != nil {
				t.Fatal(err)
			}
			balance := state.GetBalance(params.ShardStakingContractAddress)
			if locked.ToBig().Cmp(wantLocked) != 0 || balance.ToBig().Cmp(wantBalance) != 0 {
				t.Fatalf("locked stake %s, contract balance %s; want %s, %s", locked, balance, wantLocked, wantBalance)
			}
		}
		rejectDiscount := func(label string) {
			t.Helper()
			m := NewPoWMiner(s, PoWMinerConfig{})
			work := getWork(t, m, coinbase)
			if work.Difficulty.Int64() != fullDifficulty {
				t.Fatalf("%s: required difficulty %s, want %d", label, work.Difficulty, fullDifficulty)
			}
			// Change only the local search target; the served header keeps its base
			// difficulty. Import must recompute eligibility from the parent state
			// and window, then reject this valid difficulty-4 seal at difficulty 32.
			work.Difficulty = big.NewInt(reducedDifficulty)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			nonce, mix, ok := search(ctx, work)
			if !ok {
				t.Fatal("discounted nonce search canceled")
			}
			head := chain.CurrentBlock()
			if err := m.SubmitWork(work.SealHash, nonce, mix); err == nil || err.Error() != "invalid Ethash proof of work" {
				t.Fatalf("%s: discounted block error %v, want invalid Ethash proof of work", label, err)
			}
			if chain.CurrentBlock().Hash() != head.Hash() || len(reported) != 0 {
				t.Fatalf("%s: rejected block changed the head or was reported", label)
			}
			t.Logf("%s: block=#%d searchedDifficulty=%d requiredDifficulty=%d nonce=%d validation=rejected (invalid Ethash proof of work)", label, work.Number, reducedDifficulty, fullDifficulty, nonce.Uint64())
		}

		// #1: A's signed transfer locks real QKC in the contract. B mines the
		// transaction so A starts with both discounted blocks of quota available.
		tx := send(stake, nil)
		checkTransaction(mine("deposit 20 QKC for two blocks of quota", other, fullDifficulty), tx)
		checkStake(stake, stake)
		// #2-#3: each seal satisfies difficulty 4 but fails difficulty 32,
		// so successful import proves that the staking discount is applied.
		mine("first staked block", coinbase, reducedDifficulty)
		mine("second staked block", coinbase, reducedDifficulty)
		// #4: A has used both quota slots. Stake remains locked, but the next
		// block must satisfy full difficulty and also counts toward the window.
		rejectDiscount("quota exhausted")
		mine("quota exhausted", coinbase, fullDifficulty)

		// #5-#6: candidate #6 sees preceding blocks [A3, A4, B5], so A4 must
		// still consume quota despite being ordinary PoW. After #6 it becomes
		// [A4, B5, B6], leaving room for A's next discounted block.
		mine("advance window", other, fullDifficulty)
		rejectDiscount("ordinary PoW block still consumes quota")
		mine("advance window", other, fullDifficulty)
		// #7 uses the locked stake in parent #6. Executing unlock() in #7
		// changes eligibility only for its children, so #7 still gets the discount.
		tx = send(new(big.Int), crypto.Keccak256([]byte("unlock()"))[:4])
		checkTransaction(mine("quota restored; unlock uses parent stake", coinbase, reducedDifficulty), tx)
		// Unlock disables the discount immediately while funds remain in the
		// contract. Only one A block is in the preceding window, isolating the
		// loss of locked stake from exhaustion of the original two-block quota.
		checkStake(new(big.Int), stake)
		rejectDiscount("stake unlocked with quota still available")

		// #8: the real contract requires three days after unlock before withdrawal.
		// synctest advances the clock; contract execution and Ethash still run normally.
		time.Sleep(3 * 24 * time.Hour)
		tx = send(new(big.Int), crypto.Keccak256([]byte("withdrawAll()"))[:4])
		checkTransaction(mine("withdraw all 20 QKC", other, fullDifficulty), tx)
		checkStake(new(big.Int), new(big.Int))
		// #9's preceding window is [B6, A7, B8]; the original quota would still
		// suffice, but zero stake must force full difficulty after withdrawal.
		rejectDiscount("stake withdrawn with quota still available")
		mine("stake withdrawn", coinbase, fullDifficulty)
		t.Logf("Validated PoSW staking, quota exhaustion and recovery, and withdrawal; final head #%d", chain.CurrentBlock().Number)
	})
}
