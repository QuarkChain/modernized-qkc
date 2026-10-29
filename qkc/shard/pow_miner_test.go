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

func checkMiningCancelled(t *testing.T, work controlledMiningWork) {
	t.Helper()
	synctest.Wait()
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
		if err := m.Start(); err != nil {
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
		if err := m.Start(); !errors.Is(err, ErrPoWMinerStopped) {
			t.Fatalf("start after stop: %v", err)
		}
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
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
		// Remote engines may return an older solution through the latest result
		// channel. Refreshing work does not invalidate its parent or root.
		sealed := first.sealed(1)
		third.results <- sealed
		next := nextMiningWork(t, engine)
		env.checkHead(sealed)
		if next.block.ParentHash() != sealed.Hash() {
			t.Fatal("older result did not become the mining parent")
		}
		checkMiningCancelled(t, third)
		// Importing the result changes the head and invalidates all siblings.
		late := second.sealed(2)
		next.results <- late
		checkNoMiningWork(t, engine)
		if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
			t.Fatal("head change allowed an obsolete sibling to be imported")
		}
	})
}

func TestPoWMinerRestartRejectsOldResults(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		first := nextMiningWork(t, engine)
		m.Pause()
		m.Start()
		second := nextMiningWork(t, engine)
		if engine.SealHash(first.block.Header()) != engine.SealHash(second.block.Header()) {
			t.Fatal("expected identical candidates when resuming without advancing time")
		}
		late := first.sealed(1)
		first.results <- late
		checkNoMiningWork(t, engine)
		if env.chain.HasBlock(late.Hash(), late.NumberU64()) {
			t.Fatal("resumed miner imported a result from before the pause")
		}
		sealed := second.sealed(2)
		second.results <- sealed
		nextMiningWork(t, engine)
		env.checkHead(sealed)
	})
}

func TestPoWMinerRefreshDuringBuild(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		engine.prepare = func(*types.Header) error {
			if calls.Add(1) == 2 {
				close(entered)
				<-release
			}
			return nil
		}
		env, m, _ := newShardMiningEnv(t, engine)
		// Release a blocked build before cleanup waits for the miner to stop.
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		m.Start()
		first := nextMiningWork(t, engine)
		time.Sleep(testWorkRefreshInterval)
		<-entered
		time.Sleep(testBlockBuildBudget)
		select {
		case <-first.stop:
			t.Fatal("refresh stopped sealing before the replacement was ready")
		default:
		}
		checkNoMiningWork(t, engine)
		sealed := first.sealed(1)
		first.results <- sealed
		synctest.Wait()
		env.checkHead(sealed)
		checkMiningCancelled(t, first)
		close(release)
		next := nextMiningWork(t, engine)
		if next.block.ParentHash() != sealed.Hash() {
			t.Fatal("cancelled refresh was published after its parent changed")
		}
		checkNoMiningWork(t, engine)
	})
}

func TestPoWMinerRefreshFailure(t *testing.T) {
	for _, failure := range []string{"build", "seal"} {
		t.Run(failure, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				failed := make(chan struct{})
				var calls atomic.Int32
				failRefresh := func() error {
					if calls.Add(1) == 2 {
						close(failed)
						return errors.New("injected refresh failure")
					}
					return nil
				}
				if failure == "build" {
					engine.prepare = func(*types.Header) error { return failRefresh() }
				} else {
					engine.seal = failRefresh
				}
				env, m, _ := newShardMiningEnv(t, engine)
				m.Start()
				first := nextMiningWork(t, engine)
				time.Sleep(testWorkRefreshInterval)
				<-failed
				checkNoMiningWork(t, engine)
				select {
				case <-first.stop:
					t.Fatal("failed refresh stopped existing sealing work")
				default:
				}
				sealed := first.sealed(1)
				first.results <- sealed
				nextMiningWork(t, engine)
				env.checkHead(sealed)
			})
		})
	}
}

func TestPoWMinerRootRefreshKeepsWork(t *testing.T) {
	for _, duringBuild := range []bool{false, true} {
		name := "sealing"
		if duringBuild {
			name = "building"
		}
		t.Run(name, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				entered, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				engine.prepare = func(*types.Header) error {
					if calls.Add(1) == 2 {
						close(entered)
						<-release
					}
					return nil
				}
				env, m, _ := newShardMiningEnv(t, engine)
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				m.Start()
				first := nextMiningWork(t, engine)
				if duringBuild {
					time.Sleep(testWorkRefreshInterval)
					<-entered
				}
				root := newRoot(env.rootGenesis, 1)
				if changed, err := env.sc.InsertRootBlock(root); err != nil || !changed {
					t.Fatalf("root import: changed %v, err %v", changed, err)
				}
				if !duringBuild {
					// A root advance leaves current work alone; the next timed
					// refresh picks up the new root.
					checkNoMiningWork(t, engine)
					time.Sleep(testWorkRefreshInterval)
					<-entered
				}
				checkNoMiningWork(t, engine)
				select {
				case <-first.stop:
					t.Fatal("root advance stopped sealing before the replacement was ready")
				default:
				}
				close(release)
				next := nextMiningWork(t, engine)
				wantRoot := root.Hash()
				if duringBuild {
					// Work already under construction keeps its selected root.
					wantRoot = env.rootGenesis.Hash()
				}
				extra, _ := next.block.Header().QKCExtra()
				if next.block.ParentHash() != first.block.ParentHash() || extra.PrevRootBlock != wantRoot {
					t.Fatal("refresh changed the selected parent or root")
				}
				if next.results != first.results {
					t.Fatal("root refresh retired the result channel for pending work")
				}
				checkMiningCancelled(t, first)
				// The original candidate is still valid after both the root
				// advance and the refresh, even though its local search stopped.
				sealed := first.sealed(1)
				next.results <- sealed
				after := nextMiningWork(t, engine)
				env.checkHead(sealed)
				extra, _ = after.block.Header().QKCExtra()
				if after.block.ParentHash() != sealed.Hash() || extra.PrevRootBlock != root.Hash() {
					t.Fatal("next candidate did not use the new head and root tip")
				}
			})
		})
	}
}

func TestPoWMinerUnrelatedResults(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		work := nextMiningWork(t, engine)
		header := work.block.Header()
		header.Coinbase = common.Address{0x99}
		work.results <- work.block.WithSeal(header)
		work.results <- nil
		time.Sleep(miningRetryInterval)
		checkNoMiningWork(t, engine)
		select {
		case <-work.stop:
			t.Fatal("unrelated results stopped valid sealing work")
		default:
		}
		sealed := work.sealed(1)
		work.results <- sealed
		next := nextMiningWork(t, engine)
		env.checkHead(sealed)
		// A duplicate result must not interrupt mining on the new head either.
		next.results <- sealed
		checkNoMiningWork(t, engine)
		select {
		case <-next.stop:
			t.Fatal("duplicate result stopped valid sealing work")
		default:
		}
	})
}

func TestPoWMinerHeadChange(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		engine := newControlledMiningEngine()
		env, m, _ := newShardMiningEnv(t, engine)
		m.Start()
		first := nextMiningWork(t, engine)
		competitor := first.sealed(2)
		if err := env.sc.InsertShardBlock(competitor); err != nil {
			t.Fatal(err)
		}
		second := nextMiningWork(t, engine)
		checkMiningCancelled(t, first)
		extra, _ := second.block.Header().QKCExtra()
		if second.block.ParentHash() != competitor.Hash() || extra.PrevRootBlock != env.rootGenesis.Hash() {
			t.Fatal("candidate did not follow the new head")
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
			t.Fatal("head change allowed obsolete work to be imported")
		}
		sealed := second.sealed(3)
		second.results <- sealed
		nextMiningWork(t, engine)
		env.checkHead(sealed)
	})
}

func TestPoWMinerRootAdvance(t *testing.T) {
	for _, stage := range []string{"building", "sealing"} {
		t.Run(stage, func(t *testing.T) {
			testPoWMiner(t, func(t *testing.T) {
				engine := newControlledMiningEngine()
				entered, release := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				var calls atomic.Int32
				engine.prepare = func(*types.Header) error {
					if calls.Add(1) == 1 {
						close(entered)
						<-release
					}
					return nil
				}
				env, m, _ := newShardMiningEnv(t, engine)
				m.Start()
				<-entered
				var work controlledMiningWork
				if stage == "sealing" {
					close(release)
					work = nextMiningWork(t, engine)
				}
				newTip := newRoot(env.rootGenesis, 1)
				env.insertRoot(newTip, true)
				env.checkHead(env.genesis)
				checkNoMiningWork(t, engine)
				if stage == "building" {
					close(release)
					work = nextMiningWork(t, engine)
				}
				if calls.Load() != 1 {
					t.Fatal("root advance restarted candidate construction")
				}
				select {
				case <-work.stop:
					t.Fatal("root advance cancelled valid sealing work")
				default:
				}
				extra, _ := work.block.Header().QKCExtra()
				if extra.PrevRootBlock != env.rootGenesis.Hash() {
					t.Fatal("root advance replaced work referencing the previous root")
				}
				sealed := work.sealed(1)
				work.results <- sealed
				next := nextMiningWork(t, engine)
				env.checkHead(sealed)
				extra, _ = next.block.Header().QKCExtra()
				if next.block.ParentHash() != sealed.Hash() || extra.PrevRootBlock != newTip.Hash() {
					t.Fatal("next candidate did not use the new head and root tip")
				}
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
		m.Start()
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
	for _, failure := range []string{"build", "seal", "import"} {
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
					work.results <- work.sealed(1)
					<-failed
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
				if calls.Load() < 2 {
					t.Fatal("failed operation was not retried")
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
				m.Start()
				work := nextMiningWork(t, engine)
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
				checkMiningCancelled(t, work)
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
