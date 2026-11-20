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

// Package mining serves proof-of-work candidates and runs local seal searches.
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

// Block is the part of a root or shard block needed to schedule mining work.
type Block interface {
	ParentHash() common.Hash
	NumberU64() uint64
	Time() uint64
}

// MinerBackend supplies candidate construction, block import and head tracking
// for PoWMiner. E is the chain's native head event.
type MinerBackend[B Block, E any] interface {
	// CreateBlockToMine builds an unsealed candidate on the chain head, paying
	// coinbase, and returns the difficulty its seal must meet.
	CreateBlockToMine(ctx context.Context, coinbase common.Address) (B, *big.Int, error)

	// MiningHashes returns the seal hash and referenced root checkpoint.
	MiningHashes(block B) (sealHash, root common.Hash, err error)
	// WithMiningResult attaches the seal without modifying the served block.
	WithMiningResult(block B, nonce types.BlockNonce, mixHash common.Hash) (B, error)

	// InsertMinedBlock imports a sealed candidate.
	InsertMinedBlock(block B) error
	MiningHead() common.Hash
	// MiningRootTip returns the root checkpoint new work should reference, or zero for root mining.
	MiningRootTip() common.Hash
	SubscribeChainHeadEvent(ch chan<- E) event.Subscription
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
	// the chain head.
	ErrStaleWork = errors.New("stale mining work")
)

// MiningWork is a candidate block served for sealing.
type MiningWork struct {
	SealHash   common.Hash // Hash of the candidate without its seal
	Number     uint64      // Candidate block number
	Difficulty *big.Int    // Difficulty the seal must meet
	Time       uint64      // Candidate timestamp
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
// expires or the chain head or referenced root tip changes. Served candidates stay
// acceptable while their parent is the head, including candidates superseded by
// newer work. Seals are applied to the served candidate, which the backend
// imports.
type PoWMiner[B Block, E any] struct {
	backend MinerBackend[B, E]
	config  PoWMinerConfig

	build   chan struct{}                             // Semaphore serializing candidate construction
	mu      sync.Mutex                                // Protects works and current
	works   lru.BasicLRU[common.Hash, B]              // Served candidates by seal hash
	current lru.BasicLRU[common.Address, *servedWork] // Current work by coinbase

	lifecycle sync.Mutex
	stop      func() // Stops local mining; nil when it is not running
}

type servedWork struct {
	MiningWork
	parent  common.Hash
	root    common.Hash
	expires time.Time
}

// NewPoWMiner creates a miner on the chain backend. Local mining starts
// with StartLocalMining.
func NewPoWMiner[B Block, E any](backend MinerBackend[B, E], config PoWMinerConfig) *PoWMiner[B, E] {
	if config.WorkRefresh <= 0 {
		config.WorkRefresh = DefaultWorkRefresh
	}
	return &PoWMiner[B, E]{
		backend: backend,
		config:  config,
		build:   make(chan struct{}, 1),
		works:   lru.NewBasicLRU[common.Hash, B](miningWorkCacheSize),
		current: lru.NewBasicLRU[common.Address, *servedWork](miningWorkCacheSize),
	}
}

// GetWork returns the current work for coinbase, building it on the chain head
// if coinbase has none, its work has expired, or the chain head or root tip has
// changed since it was built.
func (m *PoWMiner[B, E]) GetWork(ctx context.Context, coinbase common.Address) (*MiningWork, error) {
	w, err := m.getWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	work := w.MiningWork
	work.Difficulty = new(big.Int).Set(w.Difficulty)
	return &work, nil
}

func (m *PoWMiner[B, E]) getWork(ctx context.Context, coinbase common.Address) (*servedWork, error) {
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
	block, diff, err := m.backend.CreateBlockToMine(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	sealHash, root, err := m.backend.MiningHashes(block)
	if err != nil {
		return nil, err
	}
	w := &servedWork{
		MiningWork: MiningWork{
			SealHash:   sealHash,
			Number:     block.NumberU64(),
			Difficulty: diff,
			Time:       block.Time(),
		},
		parent:  block.ParentHash(),
		root:    root,
		expires: time.Now().Add(m.config.WorkRefresh),
	}
	m.mu.Lock()
	m.works.Add(sealHash, block)
	m.current.Add(coinbase, w)
	m.mu.Unlock()
	return w, nil
}

// currentWork returns the unexpired work for coinbase on the chain head and root
// tip, if any. Unlike pyquarkchain, a root tip change also rebuilds the work: a
// root reorg can orphan the root block it references without moving the shard
// head, after which the candidate cannot become the head.
func (m *PoWMiner[B, E]) currentWork(coinbase common.Address) *servedWork {
	head, root := m.backend.MiningHead(), m.backend.MiningRootTip()
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.current.Get(coinbase)
	if !ok || w.parent != head || w.root != root || !time.Now().Before(w.expires) || !m.works.Contains(w.SealHash) {
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
func (m *PoWMiner[B, E]) SubmitWork(sealHash common.Hash, nonce types.BlockNonce, mixHash common.Hash) error {
	head := m.backend.MiningHead()
	m.mu.Lock()
	block, ok := m.works.Get(sealHash)
	if ok && block.ParentHash() != head {
		m.works.Remove(sealHash)
		m.mu.Unlock()
		return ErrStaleWork
	}
	m.mu.Unlock()
	if !ok {
		return ErrUnknownWork
	}
	sealed, err := m.backend.WithMiningResult(block, nonce, mixHash)
	if err != nil {
		return err
	}
	// The chain verifies the submitted seal during import.
	if err := m.backend.InsertMinedBlock(sealed); err != nil {
		return err
	}
	m.mu.Lock()
	m.works.Remove(sealHash)
	m.mu.Unlock()
	return nil
}

// StartLocalMining begins mining locally for the configured coinbase. It is a
// no-op if local mining is running. Work served to remote miners is unaffected.
func (m *PoWMiner[B, E]) StartLocalMining() error {
	if m.config.Search == nil {
		return errors.New("local mining search not configured")
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.stop != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	heads := make(chan E, 1)
	sub := m.backend.SubscribeChainHeadEvent(heads)
	changed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		watchHeads(ctx, cancel, sub, heads, changed)
	}()
	go func() {
		defer wg.Done()
		m.mine(ctx, changed)
	}()
	m.stop = func() {
		cancel()
		wg.Wait()
	}
	return nil
}

// StopLocalMining ends local mining and waits for an import in progress.
func (m *PoWMiner[B, E]) StopLocalMining() {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.stop != nil {
		m.stop()
		m.stop = nil
	}
}

// watchHeads coalesces head events into changed. It drains them independently
// of mining: SetCanonical sends them synchronously, including while the mining
// loop imports its own block.
func watchHeads[E any](ctx context.Context, cancel context.CancelFunc, sub event.Subscription, heads <-chan E, changed chan<- struct{}) {
	defer sub.Unsubscribe()
	for {
		select {
		case <-heads:
			select {
			case changed <- struct{}{}:
			default:
			}
		case <-sub.Err():
			// The blockchain is stopping.
			cancel()
			return
		case <-ctx.Done():
			return
		}
	}
}

// mine seals and imports work for the configured coinbase until ctx is done.
func (m *PoWMiner[B, E]) mine(ctx context.Context, changed <-chan struct{}) {
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
			log.Debug("Discarded stale block", "number", w.Number, "sealhash", w.SealHash)
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
// from w's parent or w expires.
func (m *PoWMiner[B, E]) search(ctx context.Context, w *servedWork, changed <-chan struct{}) (types.BlockNonce, common.Hash, bool) {
	ctx, cancel := context.WithDeadline(ctx, w.expires)
	defer cancel()
	go func() {
		for {
			select {
			case <-changed:
				// Events may predate w, such as the import of its parent.
				if m.backend.MiningHead() != w.parent {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return m.config.Search.Search(ctx, &w.MiningWork)
}
