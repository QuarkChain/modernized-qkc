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
	"encoding/binary"
	"errors"
	"math/big"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// powMinerTestShard stands in for the shard service, without binding the
// scheduler to ShardChain or implementing peer/cluster propagation here.
type powMinerTestShard struct {
	mu     sync.Mutex
	tip    *types.Header
	builds int
	waits  []bool
	added  []*types.Block
	create func(context.Context, *types.Block) (*types.Block, error)
	add    func(context.Context, *types.Block) error
}

func newTestPoWMiner(t *testing.T) (*PoWMiner, *powMinerTestShard) {
	t.Helper()
	backend := &powMinerTestShard{tip: &types.Header{Number: big.NewInt(41), Difficulty: big.NewInt(5)}}
	m, err := NewPoWMiner(backend, ethash.NewFaker(), PoWMinerConfig{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	return m, backend
}

func (b *powMinerTestShard) Config() *params.ChainConfig { return params.MergedTestChainConfig }

func (b *powMinerTestShard) GetHeader(hash common.Hash, number uint64) *types.Header {
	header := b.GetHeaderByHash(hash)
	if header != nil && header.Number.Uint64() == number {
		return header
	}
	return nil
}

func (b *powMinerTestShard) GetHeaderByNumber(number uint64) *types.Header {
	header := b.CurrentHeader()
	if header != nil && header.Number.Uint64() == number {
		return header
	}
	return nil
}

func (b *powMinerTestShard) GetHeaderByHash(hash common.Hash) *types.Header {
	header := b.CurrentHeader()
	if header != nil && header.Hash() == hash {
		return header
	}
	return nil
}

func (b *powMinerTestShard) CurrentHeader() *types.Header {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tip == nil {
		return nil
	}
	return types.CopyHeader(b.tip)
}

func (b *powMinerTestShard) setTip(tip *types.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tip = tip
}

func (b *powMinerTestShard) CreateBlockToMine(ctx context.Context, coinbase common.Address, wait bool) (*types.Block, error) {
	b.mu.Lock()
	b.builds++
	b.waits = append(b.waits, wait)
	header := &types.Header{
		ParentHash: b.tip.Hash(), Number: new(big.Int).Add(b.tip.Number, big.NewInt(1)),
		Difficulty: big.NewInt(5), Time: uint64(time.Now().Unix()), Coinbase: coinbase, GasLimit: 100_000,
		Extra: binary.BigEndian.AppendUint64(nil, uint64(b.builds)),
	}
	b.mu.Unlock()
	block := types.NewBlockWithHeader(header).WithBody(types.Body{
		Transactions: []*types.Transaction{types.NewTx(&types.LegacyTx{Nonce: 1})},
	})
	if b.create != nil {
		return b.create(ctx, block)
	}
	return block, nil
}

func (b *powMinerTestShard) AddMinedBlock(ctx context.Context, block *types.Block) error {
	if b.add != nil {
		if err := b.add(ctx, block); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.added = append(b.added, block)
	return nil
}

func (b *powMinerTestShard) counts() (builds, added int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds, len(b.added)
}

func TestPoWMinerGetWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend := newTestPoWMiner(t)
		coinbase := common.Address{1}
		work, block, err := m.GetWork(t.Context(), coinbase)
		if err != nil {
			t.Fatal(err)
		}
		if work.Hash != m.engine.SealHash(block.Header()) || work.Hash == block.Hash() || work.Height != 42 || work.Difficulty.Cmp(big.NewInt(5)) != 0 || block.Coinbase() != coinbase {
			t.Fatalf("unexpected mining work: %+v, coinbase %v", work, block.Coinbase())
		}
		// Exactly ten seconds is still a cache hit. No background task rebuilds it.
		time.Sleep(10 * time.Second)
		cached, _, err := m.GetWork(t.Context(), coinbase)
		if err != nil || cached.Hash != work.Hash {
			t.Fatalf("work at ten seconds: %+v, %v", cached, err)
		}
		time.Sleep(time.Nanosecond)
		fresh, _, err := m.GetWork(t.Context(), coinbase)
		if err != nil || fresh.Hash == work.Hash {
			t.Fatalf("expired work was not refreshed: %+v, %v", fresh, err)
		}
		otherCoinbase := common.Address{2}
		other, otherBlock, err := m.GetWork(t.Context(), otherCoinbase)
		if err != nil || other.Hash == fresh.Hash || otherBlock.Coinbase() != otherCoinbase {
			t.Fatalf("work for another coinbase: %+v, %v", other, err)
		}
		cached, _, err = m.GetWork(t.Context(), coinbase)
		if err != nil || cached.Hash != fresh.Hash {
			t.Fatalf("another coinbase replaced cached work: %+v, %v", cached, err)
		}
		// A different parent at the same height invalidates each coinbase's work.
		tip := backend.CurrentHeader()
		tip.Extra = []byte{1}
		backend.setTip(tip)
		if builds, _ := backend.counts(); builds != 3 {
			t.Fatalf("unexpected proactive build: %d", builds)
		}
		for _, address := range []common.Address{coinbase, otherCoinbase} {
			_, next, err := m.GetWork(t.Context(), address)
			if err != nil || next.ParentHash() != tip.Hash() {
				t.Fatalf("work did not follow the new parent: %v", err)
			}
		}
		time.Sleep(11 * time.Second)
		if builds, _ := backend.counts(); builds != 5 {
			t.Fatalf("rebuilt without GetWork: %d builds", builds)
		}
	})
}

func TestPoWMinerCandidateTimestamp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend := newTestPoWMiner(t)
		backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
			header := block.Header()
			header.Time -= 11
			return block.WithSeal(header), nil
		}
		first, _, err := m.GetWork(t.Context(), common.Address{})
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := m.GetWork(t.Context(), common.Address{})
		if err != nil || first.Hash == second.Hash {
			t.Fatalf("expiry did not use the candidate timestamp: %+v, %v", second, err)
		}
	})
}

func TestPoWMinerCacheEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend := newTestPoWMiner(t)
		var works []MiningWork
		for i := int64(0); i < 128; i++ {
			work, _, err := m.GetWork(t.Context(), common.BigToAddress(big.NewInt(i)))
			if err != nil {
				t.Fatal(err)
			}
			works = append(works, work)
		}
		// Touch the oldest work, then overflow both caches: the second is evicted.
		if _, _, err := m.GetWork(t.Context(), common.Address{}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.GetWork(t.Context(), common.Address{1}); err != nil {
			t.Fatal(err)
		}
		if m.currentWorks.Len() != 128 || m.workMap.Len() != 128 {
			t.Fatalf("unbounded caches: %d coinbases, %d blocks", m.currentWorks.Len(), m.workMap.Len())
		}
		if ok, err := m.SubmitWork(t.Context(), works[1].Hash, 1, common.Hash{}); ok || err != nil {
			t.Fatalf("evicted work accepted: %v, %v", ok, err)
		}
		cached, _, err := m.GetWork(t.Context(), common.Address{})
		if err != nil || cached.Hash != works[0].Hash {
			t.Fatalf("recently used work was evicted: %+v, %v", cached, err)
		}
		if builds, _ := backend.counts(); builds != 129 {
			t.Fatalf("unexpected builds: %d", builds)
		}
	})
}

func TestPoWMinerSubmitWork(t *testing.T) {
	m, backend := newTestPoWMiner(t)
	if m.Enabled() {
		t.Fatal("miner started enabled")
	}
	// Remote work is available both before Start and after Disable.
	work, block, err := m.GetWork(t.Context(), common.Address{1})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	m.Start()
	if !m.Enabled() {
		t.Fatal("Start did not enable mining")
	}
	m.Disable()
	m.Disable()
	if m.Enabled() {
		t.Fatal("Disable did not disable mining")
	}
	if ok, err := m.SubmitWork(t.Context(), common.Hash{0xff}, 0, common.Hash{}); ok || err != nil {
		t.Fatalf("unknown work accepted: %v, %v", ok, err)
	}

	txHash := block.Transactions()[0].Hash()
	// Returned work and body slices must not expose the cached candidate.
	work.Difficulty.SetInt64(999)
	block.Transactions()[0] = types.NewTx(&types.LegacyTx{Nonce: 999})
	rejected := errors.New("invalid proof")
	mixDigest := common.Hash{2}
	backend.add = func(_ context.Context, block *types.Block) error {
		if block.Transactions()[0].Hash() != txHash || block.Difficulty().Cmp(big.NewInt(5)) != 0 {
			t.Error("cached candidate was mutated")
		}
		if block.Nonce() == 1 {
			block.Transactions()[0] = nil
			return rejected
		}
		if block.Nonce() != 2 || block.MixDigest() != mixDigest || m.engine.SealHash(block.Header()) != work.Hash {
			t.Error("result was not applied to the original candidate")
		}
		return nil
	}
	if ok, err := m.SubmitWork(t.Context(), work.Hash, 1, common.Hash{1}); ok || !errors.Is(err, rejected) {
		t.Fatalf("rejected proof: %v, %v", ok, err)
	}
	cached, candidate, err := m.GetWork(t.Context(), common.Address{1})
	if err != nil || cached.Hash != work.Hash || candidate.Nonce() != 0 || candidate.MixDigest() != (common.Hash{}) {
		t.Fatalf("rejection changed cached work: %+v, %v", cached, err)
	}
	if ok, err := m.SubmitWork(t.Context(), work.Hash, 2, mixDigest); !ok || err != nil {
		t.Fatalf("valid proof rejected: %v, %v", ok, err)
	}
	if ok, err := m.SubmitWork(t.Context(), work.Hash, 2, mixDigest); ok || err != nil {
		t.Fatalf("successful work was not removed: %v, %v", ok, err)
	}
	// The per-coinbase entry still points to the consumed work; rebuild on that miss.
	fresh, _, err := m.GetWork(t.Context(), common.Address{1})
	if err != nil || fresh.Hash == work.Hash {
		t.Fatalf("consumed work was not rebuilt: %+v, %v", fresh, err)
	}
	if builds, added := backend.counts(); builds != 2 || added != 1 {
		t.Fatalf("got %d builds, %d accepted blocks", builds, added)
	}
}

func TestPoWMinerSubmitRefreshedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestPoWMiner(t)
		old, _, err := m.GetWork(t.Context(), common.Address{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second)
		fresh, _, err := m.GetWork(t.Context(), common.Address{})
		if err != nil || fresh.Hash == old.Hash {
			t.Fatalf("work was not refreshed: %+v, %v", fresh, err)
		}
		// Age refresh replaces current work, but both proofs remain submittable.
		for _, work := range []MiningWork{old, fresh} {
			if ok, err := m.SubmitWork(t.Context(), work.Hash, 1, common.Hash{}); !ok || err != nil {
				t.Fatalf("refreshed work rejected: %v, %v", ok, err)
			}
		}
	})
}

func TestPoWMinerSubmitStaleWork(t *testing.T) {
	m, backend := newTestPoWMiner(t)
	work, _, err := m.GetWork(t.Context(), common.Address{})
	if err != nil {
		t.Fatal(err)
	}
	parent := backend.CurrentHeader()
	tip := types.CopyHeader(parent)
	tip.Extra = []byte{1}
	backend.setTip(tip)
	if ok, err := m.SubmitWork(t.Context(), work.Hash, 1, common.Hash{}); ok || err != nil {
		t.Fatalf("stale work accepted: %v, %v", ok, err)
	}
	backend.setTip(parent)
	if ok, err := m.SubmitWork(t.Context(), work.Hash, 1, common.Hash{}); ok || err != nil {
		t.Fatalf("stale work was not removed: %v, %v", ok, err)
	}
	if _, added := backend.counts(); added != 0 {
		t.Fatal("stale work reached the shard layer")
	}
}

func TestPoWMinerFailures(t *testing.T) {
	for _, failure := range []string{"build error", "nil candidate", "canceled build", "missing tip"} {
		t.Run(failure, func(t *testing.T) {
			m, backend := newTestPoWMiner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
				switch failure {
				case "build error":
					return nil, errors.New("build failed")
				case "nil candidate":
					return nil, nil
				case "canceled build":
					cancel()
				}
				return block, nil
			}
			if failure == "missing tip" {
				backend.setTip(nil)
			}
			if _, _, err := m.GetWork(ctx, common.Address{}); err == nil {
				t.Fatal("mining failure was not returned")
			}
			if m.currentWorks.Len() != 0 || m.workMap.Len() != 0 {
				t.Fatal("failed build was cached")
			}
			backend.create = nil
			backend.setTip(&types.Header{Number: big.NewInt(41), Difficulty: big.NewInt(5)})
			work, _, err := m.GetWork(t.Context(), common.Address{})
			if err != nil {
				t.Fatalf("build did not recover: %v", err)
			}
			cancel()
			if ok, err := m.SubmitWork(ctx, work.Hash, 1, common.Hash{}); ok || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled submission: %v, %v", ok, err)
			}
			if _, _, err := m.GetWork(ctx, common.Address{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled request: %v", err)
			}
			if ok, err := m.SubmitWork(t.Context(), work.Hash, 1, common.Hash{}); !ok || err != nil {
				t.Fatalf("canceled submission consumed work: %v, %v", ok, err)
			}
		})
	}
}

func TestPoWMinerConcurrentGetWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend := newTestPoWMiner(t)
		started := make(chan struct{}, 8)
		release := make(chan struct{})
		backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
			started <- struct{}{}
			<-release
			return block, nil
		}
		type response struct {
			work MiningWork
			err  error
		}
		results := make(chan response, 8)
		for range 8 {
			go func() {
				work, _, err := m.GetWork(t.Context(), common.Address{1})
				results <- response{work, err}
			}()
		}
		<-started
		// Lifecycle changes do not interrupt remote candidate creation.
		m.Start()
		m.Disable()
		close(release)
		var hash common.Hash
		for i := range 8 {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			if i == 0 {
				hash = result.work.Hash
			} else if result.work.Hash != hash {
				t.Fatal("concurrent requests received different candidates")
			}
		}
		if builds, _ := backend.counts(); builds != 1 {
			t.Fatalf("built %d candidates for the same concurrent requests", builds)
		}
	})
}

func TestPoWMinerConcurrentSubmitWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend := newTestPoWMiner(t)
		work, _, err := m.GetWork(t.Context(), common.Address{1})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan *types.Block, 2)
		release := make(chan struct{})
		rejected := errors.New("invalid proof")
		backend.add = func(_ context.Context, block *types.Block) error {
			started <- block
			<-release
			if block.Nonce() == 1 {
				return rejected
			}
			return nil
		}
		type response struct {
			nonce uint64
			ok    bool
			err   error
		}
		results := make(chan response, 2)
		for nonce := uint64(1); nonce <= 2; nonce++ {
			go func() {
				ok, err := m.SubmitWork(t.Context(), work.Hash, nonce, common.Hash{byte(nonce)})
				results <- response{nonce, ok, err}
			}()
		}
		first, second := <-started, <-started
		synctest.Wait()
		if first == second || first.Nonce() == second.Nonce() || first.MixDigest() == second.MixDigest() {
			t.Fatal("concurrent submissions shared a mutable result")
		}
		first.Transactions()[0] = nil
		if second.Transactions()[0] == nil {
			t.Fatal("concurrent submissions shared body slices")
		}
		// Shard propagation holds no cache lock, and neither submission mutates
		// the candidate returned to another remote miner.
		cached, block, err := m.GetWork(t.Context(), common.Address{1})
		if err != nil || cached.Hash != work.Hash || block.Nonce() != 0 || block.MixDigest() != (common.Hash{}) || block.Transactions()[0] == nil {
			t.Fatalf("in-flight submissions changed cached work: %+v, %v", cached, err)
		}
		m.Start()
		m.Disable()
		close(release)
		for range 2 {
			result := <-results
			if result.nonce == 1 && (result.ok || !errors.Is(result.err, rejected)) {
				t.Fatalf("invalid submission: %+v", result)
			}
			if result.nonce == 2 && (!result.ok || result.err != nil) {
				t.Fatalf("valid submission: %+v", result)
			}
		}
		if ok, err := m.SubmitWork(t.Context(), work.Hash, 2, common.Hash{2}); ok || err != nil {
			t.Fatalf("accepted work was not removed: %v, %v", ok, err)
		}
	})
}
