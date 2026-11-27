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

// Package mining serves proof-of-work candidates and runs local nonce searches.
package mining

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

// MinerBackend supplies chain-specific work and tracks the mining head.
type MinerBackend interface {
	// BuildMiningWork returns a new candidate for the miner to cache.
	BuildMiningWork(ctx context.Context, coinbase common.Address) (*Candidate, error)
	// MiningHead returns the canonical head used to check a candidate's parent.
	MiningHead() common.Hash
	// MiningAnchor returns the root tip for shard work, or zero for root work.
	// GetWork uses it to decide whether to reuse a cached candidate.
	MiningAnchor() common.Hash
	// SubscribeMiningHead signals head changes on a buffered channel. A pending
	// wakeup covers further changes. It returns nil if the chain is closed.
	SubscribeMiningHead(ch chan<- struct{}) event.Subscription
}

const (
	// DefaultWorkRefresh is how long served work stays current before it is
	// rebuilt to include newer transactions, as in pyquarkchain's get_work.
	DefaultWorkRefresh = 10 * time.Second

	// miningWorkCacheSize bounds the candidates kept for submission and the
	// coinbases with current work, as pyquarkchain's work_map and current_works.
	miningWorkCacheSize = 128

	// miningRetryInterval backs off local mining after failing to get work or
	// to import a mined block.
	miningRetryInterval = 1 * time.Second
)

var (
	// ErrUnknownWork is returned when submitting work that was not served, has
	// already been imported, or has been evicted.
	ErrUnknownWork = errors.New("unknown mining work")

	// ErrStaleWork is returned when submitting work whose parent is no longer
	// the canonical head.
	ErrStaleWork = errors.New("stale mining work")
)

// MiningWork is a candidate block served for sealing.
type MiningWork struct {
	SealHash   common.Hash // Hash of the candidate without its seal
	Number     uint64      // Candidate block number
	Difficulty *big.Int    // Difficulty the seal must meet
	Time       uint64      // Candidate timestamp
}

// Candidate binds the work to its original block. Submit seals a copy and
// imports it through the chain's normal validation path.
type Candidate struct {
	MiningWork
	Parent common.Hash
	Anchor common.Hash // Referenced root for shard work, zero for root work
	Submit func(types.BlockNonce, common.Hash) error

	refreshAt time.Time // Set by PoWMiner when it caches the candidate.
}

// NonceSearch finds seals for local mining.
type NonceSearch interface {
	// Search returns a nonce and mix hash sealing work, or false if ctx is done
	// first. It must not modify work.
	Search(ctx context.Context, work *MiningWork) (types.BlockNonce, common.Hash, bool)
}

// PoWMinerConfig configures a PoWMiner.
type PoWMinerConfig struct {
	Coinbase    common.Address // Recipient of locally mined blocks
	WorkRefresh time.Duration  // Lifetime of current work; zero selects DefaultWorkRefresh
	Search      NonceSearch    // Local seal search; nil disables StartLocalMining
}

// PoWMiner serves work to remote miners and mines locally, following
// pyquarkchain's Miner. Work is built on demand, current per coinbase until it
// expires or the chain head or referenced root changes. Served candidates stay
// acceptable while their parent is the head, including candidates superseded by
// newer work. Seals are applied to the served candidate, which the backend
// imports.
type PoWMiner struct {
	backend MinerBackend
	config  PoWMinerConfig

	build   chan struct{}                             // Semaphore serializing candidate construction
	mu      sync.Mutex                                // Protects works and current
	works   lru.BasicLRU[common.Hash, *Candidate]     // Served candidates by seal hash
	current lru.BasicLRU[common.Address, common.Hash] // Current seal hash by coinbase

	lifecycle sync.Mutex
	stop      func() // Stops local mining; nil when it is not running
}

// NewPoWMiner creates a miner on the given backend. Local mining starts
// with StartLocalMining.
func NewPoWMiner(backend MinerBackend, config PoWMinerConfig) *PoWMiner {
	if config.WorkRefresh <= 0 {
		config.WorkRefresh = DefaultWorkRefresh
	}
	return &PoWMiner{
		backend: backend,
		config:  config,
		build:   make(chan struct{}, 1),
		works:   lru.NewBasicLRU[common.Hash, *Candidate](miningWorkCacheSize),
		current: lru.NewBasicLRU[common.Address, common.Hash](miningWorkCacheSize),
	}
}

// GetWork returns the current work for coinbase, building it on the chain head
// if coinbase has none, its work has expired, or the head or referenced root has
// changed since it was built.
func (m *PoWMiner) GetWork(ctx context.Context, coinbase common.Address) (*MiningWork, error) {
	w, err := m.getWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	work := w.MiningWork
	work.Difficulty = new(big.Int).Set(w.Difficulty)
	return &work, nil
}

func (m *PoWMiner) getWork(ctx context.Context, coinbase common.Address) (*Candidate, error) {
	if w := m.currentWork(coinbase); w != nil {
		return w, nil
	}
	// Build one candidate at a time, so a concurrent request for the same
	// coinbase reuses the candidate built while it waited.
	select {
	case m.build <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-m.build }()
	if w := m.currentWork(coinbase); w != nil {
		return w, nil
	}
	candidate, err := m.backend.BuildMiningWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	candidate.refreshAt = time.Now().Add(m.config.WorkRefresh)
	m.mu.Lock()
	m.works.Add(candidate.SealHash, candidate)
	m.current.Add(coinbase, candidate.SealHash)
	m.mu.Unlock()
	return candidate, nil
}

// A root reorg can orphan the root referenced by shard work without moving the shard head.
func (m *PoWMiner) currentWork(coinbase common.Address) *Candidate {
	head := m.backend.MiningHead()
	anchor := m.backend.MiningAnchor()
	m.mu.Lock()
	defer m.mu.Unlock()
	hash, ok := m.current.Get(coinbase)
	if !ok {
		return nil
	}
	w, ok := m.works.Peek(hash)
	if !ok || w.Parent != head || w.Anchor != anchor || !time.Now().Before(w.refreshAt) {
		return nil
	}
	return w
}

// SubmitWork seals the served candidate with sealHash and imports it. A
// candidate is accepted while its parent is the chain head, even after newer
// work has been served, and is dropped once imported or stale.
// TODO(sync): Retry delivery of uncommitted mined blocks independently of work:
// if import succeeds but RPCs fail, the moved head makes resubmissions stale,
// and the local mining loop logs the error and moves on.
func (m *PoWMiner) SubmitWork(sealHash common.Hash, nonce types.BlockNonce, mixHash common.Hash) error {
	head := m.backend.MiningHead()
	m.mu.Lock()
	w, ok := m.works.Get(sealHash)
	if ok && w.Parent != head {
		m.works.Remove(sealHash)
		m.mu.Unlock()
		return ErrStaleWork
	}
	m.mu.Unlock()
	if !ok {
		return ErrUnknownWork
	}
	// TODO(mining): Check the parent under the chain's import lock. Two submissions
	// can pass the check above and both be stored and propagated.
	if err := w.Submit(nonce, mixHash); err != nil {
		return err
	}
	m.mu.Lock()
	m.works.Remove(sealHash)
	m.mu.Unlock()
	return nil
}

// StartLocalMining begins mining locally for the configured coinbase. It is a
// no-op if local mining is running. Work served to remote miners is unaffected.
func (m *PoWMiner) StartLocalMining() error {
	if m.config.Search == nil {
		return errors.New("local mining search not configured")
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.stop != nil {
		return nil
	}
	// Subscribe first so head changes during the initial build wake the search.
	changed := make(chan struct{}, 1)
	sub := m.backend.SubscribeMiningHead(changed)
	if sub == nil {
		return errors.New("mining head subscription unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		defer sub.Unsubscribe()
		select {
		case <-sub.Err():
			cancel()
		case <-ctx.Done():
		}
	})
	wg.Go(func() { m.mine(ctx, changed) })
	m.stop = func() {
		cancel()
		wg.Wait()
	}
	return nil
}

// StopLocalMining ends local mining and waits for an import in progress.
func (m *PoWMiner) StopLocalMining() {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
}

// ForwardHeadEvents turns a chain's head events into wakeups on a buffered
// channel. It keeps draining events while the miner imports its own block.
func ForwardHeadEvents[T any](subscribe func(chan<- T) event.Subscription, changed chan<- struct{}) event.Subscription {
	heads := make(chan T, 1)
	sub := subscribe(heads)
	if sub == nil {
		return nil
	}
	return event.NewSubscription(func(quit <-chan struct{}) error {
		defer sub.Unsubscribe()
		for {
			select {
			case <-heads:
				select {
				case changed <- struct{}{}:
				default:
				}
			case err := <-sub.Err():
				return err
			case <-quit:
				return nil
			}
		}
	})
}

// mine seals and imports work for the configured coinbase until ctx is done.
func (m *PoWMiner) mine(ctx context.Context, changed <-chan struct{}) {
	for ctx.Err() == nil {
		w, err := m.getWork(ctx, m.config.Coinbase)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("Failed to get mining work", "err", err)
			}
			waitForMiningRetry(ctx)
			continue
		}
		nonce, mixHash, ok := m.search(ctx, w, changed)
		if !ok {
			continue
		}
		switch err := m.SubmitWork(w.SealHash, nonce, mixHash); {
		case err == nil:
			log.Info("Mined block", "number", w.Number, "sealhash", w.SealHash)
		case errors.Is(err, ErrStaleWork):
			log.Debug("Discarded stale mined block", "number", w.Number, "sealhash", w.SealHash)
		default:
			log.Warn("Failed to import mined block", "number", w.Number, "err", err)
			waitForMiningRetry(ctx)
		}
	}
}

// waitForMiningRetry waits before local mining retries a failed step.
func waitForMiningRetry(ctx context.Context) {
	select {
	case <-time.After(miningRetryInterval):
	case <-ctx.Done():
	}
}

// search runs the local search on w until it finds a seal, the head moves away
// from w's parent or the work is due for refresh.
func (m *PoWMiner) search(ctx context.Context, w *Candidate, changed <-chan struct{}) (types.BlockNonce, common.Hash, bool) {
	ctx, cancel := context.WithDeadline(ctx, w.refreshAt)
	var watcher sync.WaitGroup
	defer func() {
		cancel()
		// A finished search must not consume the next search's head change.
		watcher.Wait()
	}()
	watcher.Go(func() {
		for {
			select {
			case <-changed:
				// Events may predate w, such as the import of its parent.
				if m.backend.MiningHead() != w.Parent {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
	return m.config.Search.Search(ctx, &w.MiningWork)
}
