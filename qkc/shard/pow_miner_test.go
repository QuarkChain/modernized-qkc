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
	"errors"
	"maps"
	"math/big"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

func testPoWMiner(t *testing.T, test func(*testing.T)) {
	t.Helper()
	// Geth's process-wide sender workers outlive individual tests and must not
	// belong to a synctest bubble, which requires its goroutines to terminate.
	core.SenderCacher()
	synctest.Test(t, test)
}

func newPoWMinerEnv(t *testing.T, config PoWMinerConfig) (*testEnv, *PoWMiner) {
	t.Helper()
	env, _, _ := newMiningEnv(t, 10002)
	m := NewPoWMiner(newMiningShard(env.sc), config)
	t.Cleanup(m.StopLocalMining)
	return env, m
}

// newMiningShard acknowledges cluster delivery for the mining tests.
func newMiningShard(chain *ShardChain) *Shard {
	return &Shard{chain: chain, clusterClient: &backendClusterClient{
		broadcast: func(context.Context, *big.Int, common.Hash, []*types.DepositTx) error { return nil },
		report:    func(context.Context, *types.Header, *uint256.Int) error { return nil },
	}}
}

// servedBlock returns the served candidate with the given seal hash.
func (m *PoWMiner) servedBlock(sealHash common.Hash) *types.Block {
	m.mu.Lock()
	defer m.mu.Unlock()
	block, _ := m.works.Get(sealHash)
	return block
}

func getWork(t *testing.T, m *PoWMiner, coinbase common.Address) *MiningWork {
	t.Helper()
	work, err := m.GetWork(context.Background(), coinbase)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func waitForHead(t *testing.T, chain *core.BlockChain, number uint64) {
	t.Helper()
	for deadline := time.Now().Add(time.Hour); chain.CurrentBlock().Number.Uint64() < number; {
		if time.Now().After(deadline) {
			t.Fatalf("head #%d did not reach #%d", chain.CurrentBlock().Number.Uint64(), number)
		}
		time.Sleep(time.Second)
	}
}

func TestPoWMinerGetWork(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, PoWMinerConfig{})
		a, b := common.Address{0xaa}, common.Address{0xbb}
		first := getWork(t, m, a)
		block := m.servedBlock(first.SealHash)
		if block == nil || block.Coinbase() != a || block.ParentHash() != env.genesis.Hash() {
			t.Fatal("work was not built on the head for the requested coinbase")
		}
		if sealHash, _ := block.Header().QKCSealHash(); sealHash != first.SealHash || first.Number != 1 || first.Time != block.Time() {
			t.Fatalf("work does not describe its candidate: %+v", first)
		}
		// Without staking, the work targets the candidate's base difficulty.
		if first.Difficulty.Sign() <= 0 || first.Difficulty.Cmp(block.Header().QKCDifficulty()) != 0 {
			t.Fatalf("work difficulty %v, want base difficulty %v", first.Difficulty, block.Header().QKCDifficulty())
		}
		time.Sleep(DefaultWorkRefresh - time.Second)
		if work := getWork(t, m, a); work.SealHash != first.SealHash {
			t.Fatal("current work was rebuilt")
		}
		other := getWork(t, m, b)
		if other.SealHash == first.SealHash || m.servedBlock(other.SealHash).Coinbase() != b {
			t.Fatal("coinbases share work")
		}
		time.Sleep(time.Second)
		refreshed := getWork(t, m, a)
		if refreshed.SealHash == first.SealHash || refreshed.Time <= first.Time {
			t.Fatal("expired work was not rebuilt")
		}
		if err := m.SubmitWork(other.SealHash, types.EncodeNonce(1), common.Hash{}); err != nil {
			t.Fatal(err)
		}
		next := getWork(t, m, a)
		if next.Number != 2 || m.servedBlock(next.SealHash).ParentHash() != env.chain.CurrentBlock().Hash() {
			t.Fatal("work was not rebuilt on the new head")
		}
	})
}

func TestPoWMinerGetWorkRootReorg(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, PoWMinerConfig{})
		coinbase := common.Address{0xaa}
		first := getWork(t, m, coinbase)
		// Replace the root tip the work references, keeping the shard head.
		seed := uint64(1)
		fork := env.sc.RootBlock(env.sc.rootTip.ParentHash).Header().CreateBlockToAppend(nil, uint256.NewInt(1), nil, &seed, nil)
		env.insertRoot(fork, true)
		env.checkHead(env.genesis)
		work := getWork(t, m, coinbase)
		extra, _ := m.servedBlock(work.SealHash).Header().QKCExtra()
		if work.SealHash == first.SealHash || extra.PrevRootBlock != fork.Hash() {
			t.Fatal("work was not rebuilt on the new root tip")
		}
	})
}

func TestPoWMinerSubmitWork(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, PoWMinerConfig{})
		a, b := common.Address{0xaa}, common.Address{0xbb}
		if err := m.SubmitWork(common.Hash{1}, types.BlockNonce{}, common.Hash{}); !errors.Is(err, ErrUnknownWork) {
			t.Fatalf("unknown work: %v", err)
		}
		first := getWork(t, m, a)
		time.Sleep(DefaultWorkRefresh)
		second := getWork(t, m, a)
		other := getWork(t, m, b)
		// Superseded work is accepted while its parent is the head.
		nonce, mixHash := types.EncodeNonce(7), common.Hash{7}
		if err := m.SubmitWork(first.SealHash, nonce, mixHash); err != nil {
			t.Fatal(err)
		}
		head := env.chain.CurrentBlock()
		extra, _ := head.QKCExtra()
		sealHash, _ := head.QKCSealHash()
		if head.Number.Uint64() != 1 || head.Coinbase != a || sealHash != first.SealHash || extra.Nonce != nonce || extra.MixHash != mixHash {
			t.Fatal("head is not the sealed candidate")
		}
		if err := m.SubmitWork(first.SealHash, nonce, mixHash); !errors.Is(err, ErrUnknownWork) {
			t.Fatalf("imported work: %v", err)
		}
		for _, work := range []*MiningWork{second, other} {
			if err := m.SubmitWork(work.SealHash, nonce, mixHash); !errors.Is(err, ErrStaleWork) {
				t.Fatalf("work on the old head: %v", err)
			}
			if err := m.SubmitWork(work.SealHash, nonce, mixHash); !errors.Is(err, ErrUnknownWork) {
				t.Fatalf("dropped stale work: %v", err)
			}
		}
		env.checkHead(env.chain.GetBlockByHash(head.Hash()))
	})
}

// failingShard is a shard layer whose imports fail while err is set.
type failingShard struct {
	*Shard
	err error
}

func (s *failingShard) InsertMinedBlock(block *types.Block) error {
	if s.err != nil {
		return s.err
	}
	return s.Shard.InsertMinedBlock(block)
}

func TestPoWMinerSubmitWorkInsertFailure(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, _, _ := newMiningEnv(t, 10002)
		insertErr := errors.New("insert failed")
		backend := &failingShard{Shard: newMiningShard(env.sc), err: insertErr}
		m := NewPoWMiner(backend, PoWMinerConfig{})
		work := getWork(t, m, common.Address{0xaa})
		if err := m.SubmitWork(work.SealHash, types.EncodeNonce(1), common.Hash{}); !errors.Is(err, insertErr) {
			t.Fatalf("insert failure: %v", err)
		}
		env.checkHead(env.genesis)
		// The work stays available for another submission.
		backend.err = nil
		if err := m.SubmitWork(work.SealHash, types.EncodeNonce(1), common.Hash{}); err != nil {
			t.Fatal(err)
		}
		if env.chain.CurrentBlock().Number.Uint64() != 1 {
			t.Fatal("retried submission was not imported")
		}
	})
}

// sealing is a local search whose seals the test supplies.
type sealing struct {
	works chan *MiningWork
	seals chan types.BlockNonce
}

func newSealing() *sealing {
	return &sealing{works: make(chan *MiningWork), seals: make(chan types.BlockNonce)}
}

func (s *sealing) Search(ctx context.Context, work *MiningWork) (types.BlockNonce, common.Hash, bool) {
	select {
	case s.works <- work:
	case <-ctx.Done():
		return types.BlockNonce{}, common.Hash{}, false
	}
	select {
	case nonce := <-s.seals:
		return nonce, common.Hash{}, true
	case <-ctx.Done():
		return types.BlockNonce{}, common.Hash{}, false
	}
}

func TestPoWMinerLocal(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		coinbase := common.Address{0x42}
		search := newSealing()
		env, m := newPoWMinerEnv(t, PoWMinerConfig{Coinbase: coinbase, Search: search})
		if err := m.StartLocalMining(); err != nil {
			t.Fatal(err)
		}
		if err := m.StartLocalMining(); err != nil {
			t.Fatal(err)
		}
		if first := <-search.works; first.Number != 1 {
			t.Fatalf("local mining started at block #%d", first.Number)
		}
		search.seals <- types.EncodeNonce(1)
		second := <-search.works
		if head := env.chain.CurrentBlock(); head.Number.Uint64() != 1 || head.Coinbase != coinbase {
			t.Fatal("locally sealed block was not imported")
		}
		if second.Number != 2 {
			t.Fatalf("mining did not continue on the new head: block #%d", second.Number)
		}
		// A remote block changes the head and restarts the local search.
		remote := getWork(t, m, common.Address{0xbb})
		if err := m.SubmitWork(remote.SealHash, types.EncodeNonce(2), common.Hash{}); err != nil {
			t.Fatal(err)
		}
		third := <-search.works
		if third.Number != 3 || m.servedBlock(third.SealHash).ParentHash() != env.chain.CurrentBlock().Hash() {
			t.Fatal("local search did not follow the remote head")
		}
		// Expired work is replaced.
		refreshed := <-search.works
		if refreshed.SealHash == third.SealHash || refreshed.Number != 3 {
			t.Fatal("local search did not refresh expired work")
		}
		m.StopLocalMining()
		head := env.chain.CurrentBlock().Hash()
		time.Sleep(time.Minute)
		if env.chain.CurrentBlock().Hash() != head {
			t.Fatal("stopped miner imported a block")
		}
		if err := m.StartLocalMining(); err != nil {
			t.Fatal(err)
		}
		if work := <-search.works; work.Number != 3 {
			t.Fatal("restarted miner did not resume on the head")
		}
	})
}

func TestPoWMinerSimulated(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		config := params.NewTestQuarkChainConfig(10002, 10002)
		config.QuarkChain.ConsensusType = params.ConsensusSimulate
		alloc := core.SystemContractAllocs()
		maps.Copy(alloc, core.PoSWStakingContractAllocs(config))
		genesis := &core.Genesis{
			Config: config, Alloc: alloc, GasLimit: 12_000_000,
			Difficulty: new(big.Int), Mixhash: common.BigToHash(common.Big1),
		}
		genesis.ExtraData = (&types.QKCExtra{
			ChainID: config.ChainID, Cursor: types.XShardCursor{RootBlockHeight: 1},
			XShardGasLimit: genesis.GasLimit / params.XShardGasLimitDivisor,
		}).Encode()
		db := rawdb.NewMemoryDatabase()
		t.Cleanup(func() { db.Close() })
		engine, err := ethconfig.CreateConsensusEngine(config, db, genesis)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { engine.Close() })
		pow, ok := engine.(*posw.PoSW)
		if !ok || !pow.IsSimulate() {
			t.Fatalf("consensus engine %T, want simulated PoSW", engine)
		}
		t.Logf("Initialized consensus: chainID=%s type=%s", config.ChainID, config.QuarkChain.ConsensusType)
		// Use the selected engine for both construction and full block import.
		chain, err := core.NewBlockChain(db, genesis, engine, nil)
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
		builder := miner.New(miningBackend{chain, pool}, minerConfig, engine)

		// Reporting runs in the mining loop after import. StopLocalMining
		// waits for the callback before the test reads the collected headers.
		var headers []*types.Header
		client := &backendClusterClient{
			broadcast: func(context.Context, *big.Int, common.Hash, []*types.DepositTx) error { return nil },
			report: func(_ context.Context, header *types.Header, _ *uint256.Int) error {
				if err := pow.VerifyHeader(chain, header); err != nil {
					t.Errorf("block #%d header verification: %v", header.Number, err)
				} else {
					t.Logf("Block #%d passed header verification", header.Number)
				}
				if err := pow.VerifySeal(chain, header); err != nil {
					t.Errorf("block #%d seal verification: %v", header.Number, err)
				} else {
					t.Logf("Block #%d passed simulated seal verification", header.Number)
				}
				if head := chain.CurrentBlock(); head.Hash() != header.Hash() {
					t.Errorf("block #%d was reported without becoming head #%d", header.Number, head.Number)
				} else {
					t.Logf("Block #%d became head: hash=%s parent=%s", header.Number, header.Hash(), header.ParentHash)
				}
				headers = append(headers, header)
				return nil
			},
		}
		s, err := New(NewShardChain(db, chain, builder), client)
		if err != nil {
			t.Fatal(err)
		}
		root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
			Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1),
		}, qkctypes.ShardBlockHeaders{s.CurrentBlock()}, nil)
		if err := s.Init(root); err != nil {
			t.Fatal(err)
		}
		t.Logf("Initialized shard: genesis=%s root=%s", chain.Genesis().Hash(), root.Hash())
		coinbase := common.Address{0x42}
		m := NewPoWMiner(s, PoWMinerConfig{Coinbase: coinbase, Search: NewSimulatedSearch(5 * time.Second)})
		t.Cleanup(m.StopLocalMining)
		t.Logf("Starting simulated local mining: coinbase=%s targetBlocks=3", coinbase)
		if err := m.StartLocalMining(); err != nil {
			t.Fatal(err)
		}
		waitForHead(t, chain, 3)
		m.StopLocalMining()
		t.Logf("Stopped local mining: head=#%d reportedBlocks=%d", chain.CurrentBlock().Number, len(headers))
		if len(headers) < 3 {
			t.Fatalf("reported %d blocks, want at least 3", len(headers))
		}
		parent := chain.Genesis()
		for i, header := range headers {
			n := uint64(i + 1)
			block := chain.GetBlockByNumber(n)
			if block == nil || block.Hash() != header.Hash() || header.Number.Uint64() != n || block.ParentHash() != parent.Hash() {
				t.Fatalf("reported block #%d does not extend the canonical chain", n)
			}
			if block.Coinbase() != coinbase || block.Time() <= parent.Time() || block.Time() > uint64(time.Now().Unix()) {
				t.Fatalf("unexpected block #%d: coinbase %x, time %d after %d", n, block.Coinbase(), block.Time(), parent.Time())
			}
			parent = block
		}
		if chain.CurrentBlock().Hash() != parent.Hash() {
			t.Fatal("head is not the last reported block")
		}
		t.Logf("Validated %d consecutive canonical blocks; final head #%d (%s)", len(headers), parent.Number(), parent.Hash())
	})
}

func TestPoWMinerStartWithoutSearch(t *testing.T) {
	_, m := newPoWMinerEnv(t, PoWMinerConfig{})
	if err := m.StartLocalMining(); err == nil {
		t.Fatal("started local mining without a search")
	}
}

func TestSimulatedSearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const blockTime = 10 * time.Second
		search := NewSimulatedSearch(blockTime)
		// Seals are never earlier than the candidate's timestamp.
		work := &MiningWork{Time: uint64(time.Now().Unix()) + 5}
		if _, _, ok := search.Search(context.Background(), work); !ok || time.Now().Before(time.Unix(int64(work.Time), 0)) {
			t.Fatal("sealed before the candidate's timestamp")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, ok := search.Search(ctx, &MiningWork{Time: work.Time + 3600}); ok {
			t.Fatal("cancelled search sealed")
		}
		// Delays average the block time.
		const samples = 500
		var total time.Duration
		for range samples {
			start := time.Now()
			search.Search(context.Background(), &MiningWork{Time: uint64(start.Unix())})
			total += time.Since(time.Unix(start.Unix(), 0))
		}
		if mean := total / samples; mean < blockTime*8/10 || mean > blockTime*12/10 {
			t.Fatalf("mean delay %v, want about %v", mean, blockTime)
		}
	})
}
