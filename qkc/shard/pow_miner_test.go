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
	"math/big"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/mining"
	"github.com/holiman/uint256"
)

func testPoWMiner(t *testing.T, test func(*testing.T)) {
	t.Helper()
	// Geth's process-wide sender workers outlive individual tests and must not
	// belong to a synctest bubble, which requires its goroutines to terminate.
	core.SenderCacher()
	synctest.Test(t, test)
}

func newPoWMinerEnv(t *testing.T, config mining.PoWMinerConfig) (*testEnv, *mining.PoWMiner) {
	t.Helper()
	env, _, _ := newMiningEnv(t, 10002)
	m := mining.NewPoWMiner(newMiningShard(env.sc), config)
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

func getWork(t *testing.T, m *mining.PoWMiner, coinbase common.Address) *mining.MiningWork {
	t.Helper()
	work, err := m.GetWork(context.Background(), coinbase)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

// Submit the work through the miner and check the candidate that reached the chain.
func submitWork(t *testing.T, env *testEnv, m *mining.PoWMiner, work *mining.MiningWork) *types.Header {
	t.Helper()
	if err := m.SubmitWork(work.SealHash, types.EncodeNonce(1), common.Hash{}); err != nil {
		t.Fatal(err)
	}
	head := env.chain.CurrentBlock()
	hash, err := head.QKCSealHash()
	if err != nil {
		t.Fatal(err)
	}
	if hash != work.SealHash || head.Number.Uint64() != work.Number || head.Time != work.Time {
		t.Fatalf("imported block does not match served work: %+v", work)
	}
	return head
}

func waitForHead(t *testing.T, env *testEnv, number uint64) {
	t.Helper()
	for deadline := time.Now().Add(time.Hour); env.chain.CurrentBlock().Number.Uint64() < number; {
		if time.Now().After(deadline) {
			t.Fatalf("head #%d did not reach #%d", env.chain.CurrentBlock().Number.Uint64(), number)
		}
		time.Sleep(time.Second)
	}
}

func TestPoWMinerGetWork(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, mining.PoWMinerConfig{})
		a, b := common.Address{0xaa}, common.Address{0xbb}
		first := getWork(t, m, a)
		time.Sleep(mining.DefaultWorkRefresh - time.Second)
		if work := getWork(t, m, a); work.SealHash != first.SealHash {
			t.Fatal("current work was rebuilt")
		}
		other := getWork(t, m, b)
		if other.SealHash == first.SealHash {
			t.Fatal("coinbases share work")
		}
		time.Sleep(time.Second)
		refreshed := getWork(t, m, a)
		if refreshed.SealHash == first.SealHash || refreshed.Time <= first.Time {
			t.Fatal("expired work was not rebuilt")
		}
		head := submitWork(t, env, m, other)
		if other.Number != 1 || head.Coinbase != b || head.ParentHash != env.genesis.Hash() {
			t.Fatal("work was not built on the head for the requested coinbase")
		}
		// Without staking, the work targets the candidate's base difficulty.
		if other.Difficulty.Sign() <= 0 || other.Difficulty.Cmp(head.QKCDifficulty()) != 0 {
			t.Fatalf("work difficulty %v, want base difficulty %v", other.Difficulty, head.QKCDifficulty())
		}
		next := getWork(t, m, a)
		nextHead := submitWork(t, env, m, next)
		if next.Number != 2 || nextHead.ParentHash != head.Hash() || nextHead.Coinbase != a {
			t.Fatal("work was not rebuilt on the new head")
		}
	})
}

func TestPoWMinerGetWorkRootReorg(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, mining.PoWMinerConfig{})
		coinbase := common.Address{0xaa}
		first := getWork(t, m, coinbase)
		// Replace the root tip the work references, keeping the shard head.
		seed := uint64(1)
		fork := env.sc.RootBlock(env.sc.rootTip.ParentHash).Header().CreateBlockToAppend(nil, uint256.NewInt(1), nil, &seed, nil)
		env.insertRoot(fork, true)
		env.checkHead(env.genesis)
		work := getWork(t, m, coinbase)
		extra, _ := submitWork(t, env, m, work).QKCExtra()
		if work.SealHash == first.SealHash || extra.PrevRootBlock != fork.Hash() {
			t.Fatal("work was not rebuilt on the new root tip")
		}
	})
}

func TestPoWMinerSubmitWork(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, m := newPoWMinerEnv(t, mining.PoWMinerConfig{})
		a, b := common.Address{0xaa}, common.Address{0xbb}
		if err := m.SubmitWork(common.Hash{1}, types.BlockNonce{}, common.Hash{}); !errors.Is(err, mining.ErrUnknownWork) {
			t.Fatalf("unknown work: %v", err)
		}
		first := getWork(t, m, a)
		time.Sleep(mining.DefaultWorkRefresh)
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
		if err := m.SubmitWork(first.SealHash, nonce, mixHash); !errors.Is(err, mining.ErrUnknownWork) {
			t.Fatalf("imported work: %v", err)
		}
		for _, work := range []*mining.MiningWork{second, other} {
			if err := m.SubmitWork(work.SealHash, nonce, mixHash); !errors.Is(err, mining.ErrStaleWork) {
				t.Fatalf("work on the old head: %v", err)
			}
			if err := m.SubmitWork(work.SealHash, nonce, mixHash); !errors.Is(err, mining.ErrUnknownWork) {
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

func (s *failingShard) BuildMiningWork(ctx context.Context, coinbase common.Address) (*mining.Candidate, error) {
	candidate, err := s.Shard.BuildMiningWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	submit := candidate.Submit
	candidate.Submit = func(nonce types.BlockNonce, mixHash common.Hash) error {
		if s.err != nil {
			return s.err
		}
		return submit(nonce, mixHash)
	}
	return candidate, nil
}

func TestPoWMinerSubmitWorkInsertFailure(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		env, _, _ := newMiningEnv(t, 10002)
		insertErr := errors.New("insert failed")
		backend := &failingShard{Shard: newMiningShard(env.sc), err: insertErr}
		m := mining.NewPoWMiner(backend, mining.PoWMinerConfig{})
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
	works chan *mining.MiningWork
	seals chan types.BlockNonce
}

func newSealing() *sealing {
	return &sealing{works: make(chan *mining.MiningWork), seals: make(chan types.BlockNonce)}
}

func (s *sealing) Search(ctx context.Context, work *mining.MiningWork) (types.BlockNonce, common.Hash, bool) {
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
		env, m := newPoWMinerEnv(t, mining.PoWMinerConfig{Coinbase: coinbase, Search: search})
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
		if third.Number != 3 {
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
		if submitWork(t, env, m, third).ParentHash != head {
			t.Fatal("local search built on a different parent than the remote head")
		}
	})
}

func TestPoWMinerSimulated(t *testing.T) {
	testPoWMiner(t, func(t *testing.T) {
		coinbase := common.Address{0x42}
		env, m := newPoWMinerEnv(t, mining.PoWMinerConfig{Coinbase: coinbase, Search: mining.NewSimulatedSearch(5 * time.Second)})
		if err := m.StartLocalMining(); err != nil {
			t.Fatal(err)
		}
		waitForHead(t, env, 3)
		m.StopLocalMining()
		for n := uint64(1); n <= 3; n++ {
			block, parent := env.chain.GetBlockByNumber(n), env.chain.GetBlockByNumber(n-1)
			if block.Coinbase() != coinbase || block.Time() <= parent.Time() || block.Time() > uint64(time.Now().Unix()) {
				t.Fatalf("unexpected block #%d: coinbase %x, time %d after %d", n, block.Coinbase(), block.Time(), parent.Time())
			}
		}
	})
}

func TestPoWMinerStartWithoutSearch(t *testing.T) {
	_, m := newPoWMinerEnv(t, mining.PoWMinerConfig{})
	if err := m.StartLocalMining(); err == nil {
		t.Fatal("started local mining without a search")
	}
}
