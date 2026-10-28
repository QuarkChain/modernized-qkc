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
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
)

const (
	testBlockBuildBudget    = 2 * time.Second
	testWorkRefreshInterval = 3 * time.Second
)

type controlledMiningWork struct {
	chain   consensus.ChainHeaderReader
	block   *types.Block
	results chan<- *types.Block
	stop    <-chan struct{}
}

func (w controlledMiningWork) sealed(nonce uint64) *types.Block {
	header := w.block.Header()
	extra, _ := header.QKCExtra()
	extra.Nonce = types.EncodeNonce(nonce)
	header.Extra = extra.Encode()
	return w.block.WithSeal(header)
}

// Control only preparation, sealing and header verification. Construction,
// execution, deposit validation and shard fork choice use their real paths.
type controlledMiningEngine struct {
	noVerifyEngine
	work    chan controlledMiningWork
	prepare func(*types.Header) error
	seal    func() error
	verify  func(*types.Header) error
}

func newControlledMiningEngine() *controlledMiningEngine {
	return &controlledMiningEngine{
		noVerifyEngine: noVerifyEngine{beacon.New(ethash.NewFaker())},
		work:           make(chan controlledMiningWork, 16),
	}
}

func (e *controlledMiningEngine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	if e.prepare != nil {
		if err := e.prepare(header); err != nil {
			return err
		}
	}
	return e.Engine.Prepare(chain, header)
}

func (e *controlledMiningEngine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	if e.seal != nil {
		if err := e.seal(); err != nil {
			return err
		}
	}
	select {
	case e.work <- controlledMiningWork{chain, block, results, stop}:
	case <-stop:
	}
	return nil
}

func (e *controlledMiningEngine) SealHash(header *types.Header) common.Hash {
	hash, err := header.QKCSealHash()
	if err != nil {
		panic(err)
	}
	return hash
}

func (e *controlledMiningEngine) VerifyHeaders(_ consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	go func() {
		for _, header := range headers {
			var err error
			if e.verify != nil {
				err = e.verify(header)
			}
			select {
			case results <- err:
			case <-abort:
				return
			}
		}
	}()
	return abort, results
}

func newShardMiningEnv(t *testing.T, engine *controlledMiningEngine) (*testEnv, *PoWMiner, *txpool.TxPool) {
	t.Helper()
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(key.PublicKey): {Balance: big.NewInt(params.Ether)}}
	env := newTestEnvWithEngine(t, engine, alloc)
	poolConfig := legacypool.DefaultConfig
	poolConfig.Journal = ""
	pool, err := txpool.New(poolConfig.PriceLimit, env.chain, []txpool.SubPool{legacypool.New(poolConfig, env.chain)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	config := miner.DefaultConfig
	config.Recommit = testBlockBuildBudget
	config.PendingFeeRecipient = common.Address{0x99}
	builder := miner.New(miningBackend{env.chain, pool}, config, engine)
	env.sc = NewShardChain(env.db, env.chain, builder)
	if err := env.sc.Init(env.rootGenesis); err != nil {
		t.Fatal(err)
	}
	m, err := NewPoWMiner(env.sc, common.Address{0x42}, testWorkRefreshInterval)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop() })
	return env, m, pool
}

func nextMiningWork(t *testing.T, engine *controlledMiningEngine) controlledMiningWork {
	t.Helper()
	select {
	case work := <-engine.work:
		return work
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for sealing work")
		return controlledMiningWork{}
	}
}

func nextMiningWorks(t *testing.T, engine *controlledMiningEngine, coinbases ...common.Address) map[common.Address]controlledMiningWork {
	t.Helper()
	works := make(map[common.Address]controlledMiningWork, len(coinbases))
	for _, coinbase := range coinbases {
		works[coinbase] = controlledMiningWork{}
	}
	for range len(works) {
		work := nextMiningWork(t, engine)
		coinbase := work.block.Coinbase()
		if previous, ok := works[coinbase]; !ok || previous.block != nil {
			t.Fatalf("unexpected or duplicate work for coinbase %s", coinbase)
		}
		works[coinbase] = work
	}
	return works
}

func checkMiningCancelled(t *testing.T, work controlledMiningWork) {
	t.Helper()
	select {
	case <-work.stop:
	default:
		t.Fatal("obsolete sealing work was not cancelled")
	}
}

func checkNoMiningWork(t *testing.T, engine *controlledMiningEngine) {
	t.Helper()
	synctest.Wait()
	select {
	case work := <-engine.work:
		t.Fatalf("unexpected sealing work for block #%d", work.block.NumberU64())
	default:
	}
}

func testPoWMiner(t *testing.T, test func(*testing.T)) {
	t.Helper()
	// Geth's process-wide sender workers outlive individual tests and must not
	// belong to a synctest bubble, which requires its goroutines to terminate.
	core.SenderCacher()
	synctest.Test(t, test)
}

func TestPoWMinerLifecycle(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		checkNoMiningWork(t, engine)
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		first := nextMiningWork(t, engine)
		if first.chain != env.chain || first.block.ParentHash() != env.genesis.Hash() || first.block.Coinbase() != (common.Address{0x42}) {
			t.Fatal("candidate did not use the configured chain, parent and fee recipient")
		}
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		checkNoMiningWork(t, engine)
		sealed := first.sealed(1)
		first.results <- sealed
		second := nextMiningWork(t, engine)
		env.checkHead(sealed)
		if second.block.ParentHash() != sealed.Hash() {
			t.Fatal("mining did not follow the imported canonical head")
		}
		if second.block.Time() <= sealed.Time() || second.block.Time() > uint64(time.Now().Unix()) {
			t.Fatal("mining timestamp must advance past the parent without running ahead of the clock")
		}
		checkMiningCancelled(t, first)
		if err := m.Pause(); err != nil {
			t.Fatal(err)
		}
		checkMiningCancelled(t, second)
		late := second.sealed(2)
		second.results <- late
		time.Sleep(2 * testWorkRefreshInterval)
		checkNoMiningWork(t, engine)
		if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
			t.Fatal("paused miner imported a late result")
		}
		if err := m.Resume(); err != nil {
			t.Fatal(err)
		}
		third := nextMiningWork(t, engine)
		if third.block.ParentHash() != sealed.Hash() {
			t.Fatal("resumed miner selected the wrong parent")
		}
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
		checkMiningCancelled(t, third)
		third.results <- third.sealed(3)
		checkNoMiningWork(t, engine)
		env.checkHead(sealed)
		if err := m.Resume(); !errors.Is(err, ErrPoWMinerStopped) {
			t.Fatalf("resume after stop: %v", err)
		}
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPoWMinerCoinbases(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		a, b := common.Address{0x42}, common.Address{0x43}
		if err := m.SetCoinbases([]common.Address{a, b, a}); err != nil {
			t.Fatal(err)
		}
		checkNoMiningWork(t, engine)
		m.Start()
		works := nextMiningWorks(t, engine, a, b)
		if engine.SealHash(works[a].block.Header()) == engine.SealHash(works[b].block.Header()) {
			t.Fatal("different coinbases shared a sealing candidate")
		}
		checkNoMiningWork(t, engine)
		if err := m.SetCoinbases([]common.Address{a}); err != nil {
			t.Fatal(err)
		}
		checkMiningCancelled(t, works[b])
		if err := m.SetCoinbases([]common.Address{a, b}); err != nil {
			t.Fatal(err)
		}
		replacement := nextMiningWork(t, engine)
		if replacement.block.Coinbase() != b {
			t.Fatal("retained coinbase work was rebuilt")
		}
		// Re-adding the address can produce the same seal hash. The cancelled
		// task's result must still be rejected using its task identity.
		late := works[b].sealed(1)
		works[b].results <- late
		synctest.Wait()
		if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
			t.Fatal("removed coinbase work was imported after re-adding its address")
		}
		// An address must not consume another address's result or cancel its work.
		replacement.results <- works[a].sealed(2)
		synctest.Wait()
		time.Sleep(miningRetryInterval)
		replacement = nextMiningWork(t, engine)
		if replacement.block.Coinbase() != b {
			t.Fatal("unrelated result retried the wrong coinbase")
		}
		select {
		case <-works[a].stop:
			t.Fatal("another coinbase's retry cancelled retained work")
		default:
		}
		env.checkHead(env.genesis)
		block := replacement.sealed(3)
		replacement.results <- block
		works = nextMiningWorks(t, engine, a, b)
		env.checkHead(block)
		for _, work := range works {
			if work.block.ParentHash() != block.Hash() {
				t.Fatal("coinbase work did not follow the imported head")
			}
		}
		if err := m.SetCoinbases(nil); err != nil {
			t.Fatal(err)
		}
		for _, work := range works {
			checkMiningCancelled(t, work)
			work.results <- work.sealed(4)
		}
		time.Sleep(2 * testWorkRefreshInterval)
		checkNoMiningWork(t, engine)
		env.checkHead(block)
		m.SetCoinbases([]common.Address{a})
		if work := nextMiningWork(t, engine); work.block.Coinbase() != a {
			t.Fatal("adding an address to an empty running miner did not publish work")
		}
		m.Stop()
		if err := m.SetCoinbases([]common.Address{a}); !errors.Is(err, ErrPoWMinerStopped) {
			t.Fatalf("set coinbases after stop: %v", err)
		}
	})
}

func TestPoWMinerCoinbaseRefresh(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		_, m, _ := newShardMiningEnv(t, engine)
		a, b := common.Address{0x42}, common.Address{0x43}
		m.Start()
		first := nextMiningWork(t, engine)
		time.Sleep(time.Second)
		m.SetCoinbases([]common.Address{a, b})
		second := nextMiningWork(t, engine)
		if second.block.Coinbase() != b {
			t.Fatal("new coinbase did not receive its own candidate")
		}
		time.Sleep(testWorkRefreshInterval - time.Second)
		refresh := nextMiningWork(t, engine)
		if refresh.block.Coinbase() != a {
			t.Fatal("first coinbase did not refresh at its own deadline")
		}
		checkMiningCancelled(t, first)
		select {
		case <-second.stop:
			t.Fatal("refresh cancelled another coinbase's work")
		default:
		}
		time.Sleep(time.Second)
		refresh = nextMiningWork(t, engine)
		if refresh.block.Coinbase() != b {
			t.Fatal("second coinbase did not refresh at its own deadline")
		}
		checkMiningCancelled(t, second)
	})
}

func TestPoWMinerCoinbaseBuildFailure(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		a, b := common.Address{0x42}, common.Address{0x43}
		var calls atomic.Int32
		engine.prepare = func(header *types.Header) error {
			if header.Coinbase == b && calls.Add(1) == 1 {
				return errors.New("injected coinbase build failure")
			}
			return nil
		}
		_, m, _ := newShardMiningEnv(t, engine)
		m.SetCoinbases([]common.Address{a, b})
		m.Start()
		work := nextMiningWork(t, engine)
		if work.block.Coinbase() != a {
			t.Fatal("failed coinbase prevented the other address from publishing work")
		}
		checkNoMiningWork(t, engine)
		time.Sleep(miningRetryInterval)
		retry := nextMiningWork(t, engine)
		if retry.block.Coinbase() != b {
			t.Fatal("build failure did not retry the affected coinbase")
		}
		select {
		case <-work.stop:
			t.Fatal("failed build cancelled another coinbase's work")
		default:
		}
	})
}

func TestPoWMinerCoinbaseBuildQueue(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "slow"
		if remove {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				a, b := common.Address{0x42}, common.Address{0x43}
				entered, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				engine.prepare = func(header *types.Header) error {
					calls.Add(1)
					if header.Coinbase == b {
						close(entered)
						<-release
					}
					return nil
				}
				_, m, _ := newShardMiningEnv(t, engine)
				m.Start()
				first := nextMiningWork(t, engine)
				m.SetCoinbases([]common.Address{a, b})
				<-entered
				if remove {
					m.SetCoinbases([]common.Address{a})
				}
				time.Sleep(2 * testWorkRefreshInterval)
				checkMiningCancelled(t, first)
				checkNoMiningWork(t, engine)
				if calls.Load() != 2 {
					t.Fatal("started overlapping builds for different coinbases")
				}
				close(release)
				coinbases := []common.Address{a}
				if !remove {
					coinbases = append(coinbases, b)
				}
				nextMiningWorks(t, engine, coinbases...)
				if calls.Load() != 3 {
					t.Fatal("queued work did not build after slow construction finished")
				}
			})
		})
	}
}

func TestPoWMinerWorkRefresh(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, pool := newShardMiningEnv(t, engine)
		m.Start()
		first := nextMiningWork(t, engine)
		key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		tx := types.MustSignNewTx(key, types.LatestSigner(env.config), &types.DynamicFeeTx{
			ChainID: env.config.ChainID, Gas: params.TxGas, To: &common.Address{0x55},
			GasFeeCap: big.NewInt(2 * params.GWei), GasTipCap: big.NewInt(params.GWei),
		})
		if err := pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
			t.Fatal(err)
		}
		gasCeil := env.genesis.GasLimit() / 2
		env.sc.builder.SetGasCeil(gasCeil)
		if err := env.sc.builder.SetGasTip(big.NewInt(2 * params.GWei)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(testBlockBuildBudget)
		checkNoMiningWork(t, engine)
		time.Sleep(testWorkRefreshInterval - testBlockBuildBudget)
		refreshAt := time.Now()
		second := nextMiningWork(t, engine)
		if !time.Now().Equal(refreshAt) {
			t.Fatal("work was not rebuilt at the configured refresh interval")
		}
		if second.block.GasLimit() != core.CalcGasLimit(env.genesis.GasLimit(), gasCeil) || len(second.block.Transactions()) != 1 {
			t.Fatal("work refresh did not use the bound builder's updated gas ceiling and tip filter")
		}
		checkMiningCancelled(t, first)
		if err := env.sc.builder.SetGasTip(big.NewInt(params.GWei)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(testWorkRefreshInterval)
		third := nextMiningWork(t, engine)
		if txs := third.block.Transactions(); len(txs) != 2 || txs[1].Hash() != tx.Hash() {
			t.Fatal("work refresh did not incorporate the pool transaction after restoring the builder tip")
		}
		checkMiningCancelled(t, second)
		late := first.sealed(1)
		first.results <- late
		synctest.Wait()
		if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
			t.Fatal("work refresh allowed obsolete work to be imported")
		}
		sealed := third.sealed(2)
		third.results <- sealed
		next := nextMiningWork(t, engine)
		env.checkHead(sealed)
		if next.block.ParentHash() != sealed.Hash() {
			t.Fatal("refreshed result did not become the mining parent")
		}
	})
}

func TestPoWMinerTipChanges(t *testing.T) {
	for _, rootChange := range []bool{false, true} {
		name := "shard"
		if rootChange {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				env, m, _ := newShardMiningEnv(t, engine)
				a, b := common.Address{0x42}, common.Address{0x43}
				m.SetCoinbases([]common.Address{a, b})
				m.Start()
				works := nextMiningWorks(t, engine, a, b)
				first := works[a]
				parent, root := env.genesis.Hash(), env.rootGenesis.Hash()
				if rootChange {
					newTip := newRoot(env.rootGenesis, 1)
					if changed, err := env.sc.InsertRootBlock(newTip); err != nil || !changed {
						t.Fatalf("root import: changed %v, err %v", changed, err)
					}
					root = newTip.Hash()
					env.checkHead(env.genesis)
				} else {
					competitor := first.sealed(2)
					if err := env.sc.InsertShardBlock(competitor); err != nil {
						t.Fatal(err)
					}
					parent = competitor.Hash()
				}
				refreshed := nextMiningWorks(t, engine, a, b)
				second := refreshed[a]
				for _, work := range works {
					checkMiningCancelled(t, work)
				}
				extra, _ := second.block.Header().QKCExtra()
				if second.block.ParentHash() != parent || extra.PrevRootBlock != root {
					t.Fatal("candidate did not follow the new tips")
				}
				late := first.sealed(1)
				first.results <- late
				// Also exercise the final freshness check independently of event delivery.
				oldExtra, _ := first.block.Header().QKCExtra()
				if err := m.importResult(miner.QKCBlockArgs{Parent: first.block.ParentHash(), PrevRootBlock: oldExtra.PrevRootBlock}, late); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
					t.Fatal("tip change allowed obsolete work to be imported")
				}
				sealed := second.sealed(3)
				second.results <- sealed
				nextMiningWorks(t, engine, a, b)
				env.checkHead(sealed)
			})
		})
	}
}

func TestPoWMinerCancelBuild(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		engine.prepare = func(*types.Header) error {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return nil
		}
		_, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		<-entered
		if err := m.Pause(); err != nil {
			t.Fatal(err)
		}
		m.Resume()
		time.Sleep(2 * testWorkRefreshInterval)
		checkNoMiningWork(t, engine)
		if calls.Load() != 1 {
			t.Fatal("started overlapping builds while cancelled preparation was pending")
		}
		close(release)
		nextMiningWork(t, engine)
		if calls.Load() != 2 {
			t.Fatalf("preparation calls: have %d, want 2", calls.Load())
		}
	})
}

func TestPoWMinerSlowBuild(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		engine.prepare = func(*types.Header) error {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return nil
		}
		_, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		<-entered
		time.Sleep(2 * testWorkRefreshInterval)
		close(release)
		work := nextMiningWork(t, engine)
		if calls.Load() != 1 {
			t.Fatal("work refresh discarded slow construction before it could publish work")
		}
		time.Sleep(testWorkRefreshInterval)
		nextMiningWork(t, engine)
		checkMiningCancelled(t, work)
	})
}

func TestPoWMinerFailureRecovery(t *testing.T) {
	for _, failure := range []string{"build", "seal", "import", "unrelated", "nil"} {
		t.Run(failure, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				failed := make(chan struct{})
				var calls atomic.Int32
				failOnce := func() error {
					if calls.Add(1) == 1 {
						close(failed)
						return errors.New("injected mining failure")
					}
					return nil
				}
				switch failure {
				case "build":
					engine.prepare = func(*types.Header) error { return failOnce() }
				case "seal":
					engine.seal = failOnce
				case "import":
					engine.verify = func(*types.Header) error { return failOnce() }
				}
				env, m, _ := newShardMiningEnv(t, engine)
				m.Start()
				if failure == "build" || failure == "seal" {
					<-failed
				} else {
					work := nextMiningWork(t, engine)
					switch failure {
					case "import":
						work.results <- work.sealed(1)
						<-failed
					case "unrelated":
						header := work.block.Header()
						header.Coinbase = common.Address{0x99}
						work.results <- work.block.WithSeal(header)
					case "nil":
						work.results <- nil
					}
				}
				synctest.Wait()
				retryAt := time.Now().Add(miningRetryInterval)
				time.Sleep(miningRetryInterval - time.Nanosecond)
				checkNoMiningWork(t, engine)
				time.Sleep(time.Nanosecond)
				work := nextMiningWork(t, engine)
				if !time.Now().Equal(retryAt) {
					t.Fatal("failure retry did not use its own interval")
				}
				env.checkHead(env.genesis)
				sealed := work.sealed(2)
				work.results <- sealed
				nextMiningWork(t, engine)
				env.checkHead(sealed)
				if failure == "build" || failure == "seal" || failure == "import" {
					if calls.Load() < 2 {
						t.Fatal("failed operation was not retried")
					}
				}
			})
		})
	}
}

func TestPoWMinerWaitForImport(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "pause"
		if stop {
			name = "stop"
		}
		t.Run(name, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				entered, release := make(chan struct{}), make(chan struct{})
				engine.verify = func(*types.Header) error {
					close(entered)
					<-release
					return nil
				}
				env, m, _ := newShardMiningEnv(t, engine)
				a, b := common.Address{0x42}, common.Address{0x43}
				m.SetCoinbases([]common.Address{a, b})
				m.Start()
				works := nextMiningWorks(t, engine, a, b)
				work := works[b]
				sealed := work.sealed(1)
				work.results <- sealed
				<-entered
				done := make(chan error, 1)
				go func() {
					if stop {
						done <- m.Stop()
					} else {
						done <- m.Pause()
					}
				}()
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("control returned while import was still running")
				default:
				}
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				env.checkHead(sealed)
				for _, work := range works {
					checkMiningCancelled(t, work)
				}
				checkNoMiningWork(t, engine)
			})
		})
	}
}

func TestPoWMinerStopWaitsForBuild(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		entered, release := make(chan struct{}), make(chan struct{})
		engine.prepare = func(*types.Header) error {
			close(entered)
			<-release
			return nil
		}
		_, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		<-entered
		done := make(chan error, 1)
		go func() { done <- m.Stop() }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("stop returned before construction ended")
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		checkNoMiningWork(t, engine)
	})
}

func TestPoWMinerInitialization(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		env.sc.mu.Lock()
		env.sc.rootTip = nil
		env.sc.mu.Unlock()
		m.Start()
		time.Sleep(2 * miningRetryInterval)
		checkNoMiningWork(t, engine)
		if err := env.sc.Init(env.rootGenesis); err != nil {
			t.Fatal(err)
		}
		nextMiningWork(t, engine)
	})
}
