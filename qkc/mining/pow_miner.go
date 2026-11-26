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
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// MinerBackend supplies shard candidates and imports sealed shard blocks.
type MinerBackend interface {
	// CreateBlockToMine builds an unsealed candidate on the shard head, paying
	// coinbase, and returns the difficulty its seal must meet.
	CreateBlockToMine(ctx context.Context, coinbase common.Address) (*types.Block, *big.Int, error)

	// InsertMinedBlock imports a sealed candidate.
	InsertMinedBlock(block *types.Block) error

	// CurrentBlock returns the shard head.
	CurrentBlock() *types.Header

	// RootTip returns the hash of the root tip, which new candidates reference.
	RootTip() common.Hash

	// SubscribeChainHeadEvent registers a subscription for shard head changes.
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
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
	config    PoWMinerConfig
	buildWork func(context.Context, common.Address) (*servedWork, error)
	head      func() common.Hash
	rootTip   func() common.Hash // Shard candidates also reference a root.
	subscribe func(chan<- core.ChainHeadEvent) event.Subscription

	build   chan struct{}                             // Semaphore serializing candidate construction
	mu      sync.Mutex                                // Protects works and current
	works   lru.BasicLRU[common.Hash, *servedWork]    // Served candidates by seal hash
	current lru.BasicLRU[common.Address, common.Hash] // Current seal hash by coinbase

	lifecycle sync.Mutex
	stop      func() // Stops local mining; nil when it is not running
}

type servedWork struct {
	MiningWork
	parent  common.Hash
	root    common.Hash
	expires time.Time
	submit  func(types.BlockNonce, common.Hash) error
}

// NewPoWMiner creates a miner on the shard layer backend. Local mining starts
// with StartLocalMining.
func NewPoWMiner(backend MinerBackend, config PoWMinerConfig) *PoWMiner {
	m := newPoWMiner(config)
	m.head = func() common.Hash { return backend.CurrentBlock().Hash() }
	m.rootTip = backend.RootTip
	m.subscribe = backend.SubscribeChainHeadEvent
	m.buildWork = func(ctx context.Context, coinbase common.Address) (*servedWork, error) {
		return buildShardWork(ctx, backend, coinbase)
	}
	return m
}

// RootBackend supplies unsigned root candidates and imports sealed root blocks.
type RootBackend interface {
	Ready() bool
	RootTip() *qkctypes.RootBlockHeader
	CreateBlockToMine(context.Context, common.Address) (*qkctypes.RootBlock, error)
	InsertBlock(*qkctypes.RootBlock) error
}

// NewRootPoWMiner serves unsigned root work at the header's base difficulty.
func NewRootPoWMiner(backend RootBackend, config PoWMinerConfig) *PoWMiner {
	m := newPoWMiner(config)
	m.head = func() common.Hash {
		if !backend.Ready() {
			return common.Hash{}
		}
		return backend.RootTip().Hash()
	}
	m.buildWork = func(ctx context.Context, coinbase common.Address) (*servedWork, error) {
		block, err := backend.CreateBlockToMine(ctx, coinbase)
		if err != nil {
			return nil, err
		}
		header := block.Header()
		return &servedWork{
			MiningWork: MiningWork{
				SealHash: header.SealHash(), Number: block.NumberU64(),
				Difficulty: header.Difficulty.ToBig(), Time: header.Time,
			},
			parent: header.ParentHash,
			submit: func(nonce types.BlockNonce, mix common.Hash) error {
				// TODO(mining): Offer discounted work to eligible root PoSW miners and
				// accept the staking signer's signature over the seal hash on submission.
				return backend.InsertBlock(block.WithMiningResult(nonce.Uint64(), mix, nil))
			},
		}, nil
	}
	// TODO(mining): Wake local root searches when the head changes. They currently
	// refresh at WorkRefresh, and submission checks the head before import.
	return m
}

func newPoWMiner(config PoWMinerConfig) *PoWMiner {
	if config.WorkRefresh <= 0 {
		config.WorkRefresh = DefaultWorkRefresh
	}
	return &PoWMiner{
		config:  config,
		build:   make(chan struct{}, 1),
		works:   lru.NewBasicLRU[common.Hash, *servedWork](miningWorkCacheSize),
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

func (m *PoWMiner) getWork(ctx context.Context, coinbase common.Address) (*servedWork, error) {
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
	w, err := m.buildWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	// TODO(mining): Recheck the head and referenced root if they change during construction.
	w.expires = time.Now().Add(m.config.WorkRefresh)
	m.mu.Lock()
	m.works.Add(w.SealHash, w)
	m.current.Add(coinbase, w.SealHash)
	m.mu.Unlock()
	return w, nil
}

func buildShardWork(ctx context.Context, backend MinerBackend, coinbase common.Address) (*servedWork, error) {
	block, diff, err := backend.CreateBlockToMine(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	header := block.Header()
	sealHash, err := header.QKCSealHash()
	if err != nil {
		return nil, err
	}
	extra, err := header.QKCExtra()
	if err != nil {
		return nil, err
	}
	return &servedWork{
		MiningWork: MiningWork{
			SealHash:   sealHash,
			Number:     block.NumberU64(),
			Difficulty: diff,
			Time:       block.Time(),
		},
		parent: block.ParentHash(),
		root:   extra.PrevRootBlock,
		submit: func(nonce types.BlockNonce, mixHash common.Hash) error {
			header, sealed := block.Header(), *extra
			sealed.Nonce, sealed.MixHash = nonce, mixHash
			header.Extra = sealed.Encode()
			return backend.InsertMinedBlock(block.WithSeal(header))
		},
	}, nil
}

// A root reorg can orphan the root referenced by shard work without moving the shard head.
func (m *PoWMiner) currentWork(coinbase common.Address) *servedWork {
	head := m.head()
	var root common.Hash
	if m.rootTip != nil {
		root = m.rootTip()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hash, ok := m.current.Get(coinbase)
	if !ok {
		return nil
	}
	w, ok := m.works.Peek(hash)
	if !ok || w.parent != head || w.root != root || !time.Now().Before(w.expires) {
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
	head := m.head()
	m.mu.Lock()
	w, ok := m.works.Get(sealHash)
	if ok && w.parent != head {
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
	if err := w.submit(nonce, mixHash); err != nil {
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
	ctx, cancel := context.WithCancel(context.Background())
	changed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	if m.subscribe != nil {
		heads := make(chan core.ChainHeadEvent, 1)
		sub := m.subscribe(heads)
		wg.Go(func() { watchHeads(ctx, cancel, sub, heads, changed) })
	}
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

// watchHeads coalesces head events into changed. It drains them independently
// of mining: SetCanonical sends them synchronously, including while the mining
// loop imports its own block.
func watchHeads(ctx context.Context, cancel context.CancelFunc, sub event.Subscription, heads <-chan core.ChainHeadEvent, changed chan<- struct{}) {
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
// from w's parent or w expires.
func (m *PoWMiner) search(ctx context.Context, w *servedWork, changed <-chan struct{}) (types.BlockNonce, common.Hash, bool) {
	ctx, cancel := context.WithDeadline(ctx, w.expires)
	defer cancel()
	go func() {
		for {
			select {
			case <-changed:
				// Events may predate w, such as the import of its parent.
				if m.head() != w.parent {
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
