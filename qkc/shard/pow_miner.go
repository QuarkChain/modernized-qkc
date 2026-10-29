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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	miningWorkCacheSize = 128
	miningWorkMaxAge    = 10 * time.Second
)

// PoWMinerBackend is the shard layer's interface to the mining scheduler.
// Implementations own synchronization with shard chain and cluster state.
type PoWMinerBackend interface {
	// The shard layer supplies the chain view passed to consensus.Engine.Seal.
	consensus.ChainHeaderReader

	// CreateBlockToMine creates a candidate for coinbase. Local mining sets wait
	// to wait for shard readiness until ctx is canceled; remote requests fail
	// immediately if mining is unavailable. A nil block ends local mining.
	CreateBlockToMine(ctx context.Context, coinbase common.Address, wait bool) (*types.Block, error)

	// AddMinedBlock validates the result, broadcasts it to peers, delivers cross-
	// shard deposits to neighbors, and reports the header to master before inserting
	// the block into the shard chain. It must recheck whether the block is still
	// acceptable: the tip can change after the scheduler's preliminary stale check.
	AddMinedBlock(ctx context.Context, block *types.Block) error
}

// MiningWork identifies a candidate and its proof-of-work target.
type MiningWork struct {
	Hash       common.Hash
	Height     uint64
	Difficulty *big.Int
}

// PoWMinerConfig selects remote work distribution or local mining. Simulation
// replaces the local consensus engine's nonce search with a randomized delay.
type PoWMinerConfig struct {
	Remote          bool
	Coinbase        common.Address // Local mining address; resolved by the shard layer.
	Simulate        bool           // The shard's consensus must accept simulated seals.
	TargetBlockTime time.Duration  // Mean simulation interval before gas usage adjustment.
}

// PoWMiner schedules shard mining through the shard layer. Remote candidates
// are refreshed by GetWork; local mining runs between Start and Disable.
type PoWMiner struct {
	backend PoWMinerBackend
	engine  consensus.Engine
	config  PoWMinerConfig

	buildMu sync.Mutex // Serializes candidate creation, including cache lookup.
	mu      sync.Mutex // Protects lifecycle state and both caches; never held during backend calls.
	enabled bool
	cancel  context.CancelFunc
	done    chan struct{}

	currentWorks lru.BasicLRU[common.Address, common.Hash]
	workMap      lru.BasicLRU[common.Hash, *types.Block]
}

// NewPoWMiner creates a disabled scheduler. The shard layer supplies the same
// engine used to build and validate its blocks. Local PoW also uses its Seal method.
func NewPoWMiner(backend PoWMinerBackend, engine consensus.Engine, config PoWMinerConfig) (*PoWMiner, error) {
	if config.TargetBlockTime < 0 {
		return nil, errors.New("negative target block time")
	}
	return &PoWMiner{
		backend:      backend,
		engine:       engine,
		config:       config,
		currentWorks: lru.NewBasicLRU[common.Address, common.Hash](miningWorkCacheSize),
		workMap:      lru.NewBasicLRU[common.Hash, *types.Block](miningWorkCacheSize),
	}, nil
}

// Start enables mining and starts a local session unless configured for remote
// mining. Repeated calls leave a running session intact.
func (m *PoWMiner) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = true
	if m.config.Remote || m.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	go m.runLocal(ctx, cancel, m.done)
}

// Disable cancels local building and sealing without waiting for an in-flight
// shard callback to return. Remote work remains available, as in the Python miner.
func (m *PoWMiner) Disable() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// Enabled reports the mining state set by Start and Disable.
func (m *PoWMiner) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// GetWork returns work for coinbase and a copy of its candidate. It rebuilds on
// a cache miss, a different parent, or a candidate timestamp over ten seconds old.
// Older candidates remain available for submission while their parent is current.
func (m *PoWMiner) GetWork(ctx context.Context, coinbase common.Address) (MiningWork, *types.Block, error) {
	if !m.config.Remote {
		return MiningWork{}, nil, errors.New("GetWork is only available for remote mining")
	}
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return MiningWork{}, nil, err
	}

	m.mu.Lock()
	var block *types.Block
	hash, ok := m.currentWorks.Get(coinbase)
	if ok {
		block, _ = m.workMap.Get(hash)
	}
	m.mu.Unlock()

	tip := m.backend.CurrentHeader()
	if tip == nil {
		return MiningWork{}, nil, errors.New("shard mining tip unavailable")
	}
	if block == nil || block.ParentHash() != tip.Hash() || time.Since(time.Unix(int64(block.Time()), 0)) > miningWorkMaxAge {
		var err error
		block, err = m.backend.CreateBlockToMine(ctx, coinbase, false)
		if err != nil {
			return MiningWork{}, nil, err
		}
		if err := ctx.Err(); err != nil {
			return MiningWork{}, nil, err
		}
		if block == nil {
			return MiningWork{}, nil, errors.New("failed to create mining block")
		}
		hash = m.engine.SealHash(block.Header())
		m.mu.Lock()
		m.currentWorks.Add(coinbase, hash)
		m.workMap.Add(hash, block)
		m.mu.Unlock()
	}
	work := MiningWork{Hash: hash, Height: block.NumberU64(), Difficulty: block.Difficulty()}
	return work, copyMiningBlock(block, block.Header()), nil
}

// SubmitWork applies a remote result to a cached candidate and hands it to the
// shard layer. Unknown or stale work returns false without an error. A rejected
// result remains cached so another proof or a failed propagation can be retried.
func (m *PoWMiner) SubmitWork(ctx context.Context, hash common.Hash, nonce uint64, mixDigest common.Hash) (bool, error) {
	if !m.config.Remote {
		return false, errors.New("SubmitWork is only available for remote mining")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	block, ok := m.workMap.Get(hash)
	m.mu.Unlock()
	if !ok {
		return false, nil
	}
	tip := m.backend.CurrentHeader()
	if tip == nil {
		return false, errors.New("shard mining tip unavailable")
	}
	if block.ParentHash() != tip.Hash() {
		m.mu.Lock()
		m.workMap.Remove(hash)
		m.mu.Unlock()
		return false, nil
	}
	header := block.Header()
	header.Nonce, header.MixDigest = types.EncodeNonce(nonce), mixDigest
	if err := m.backend.AddMinedBlock(ctx, copyMiningBlock(block, header)); err != nil {
		return false, err
	}
	m.mu.Lock()
	m.workMap.Remove(hash)
	m.mu.Unlock()
	return true, nil
}

// copyMiningBlock isolates the header and body slices while sharing immutable
// transactions, following Geth's block copying conventions.
func copyMiningBlock(block *types.Block, header *types.Header) *types.Block {
	return block.WithSeal(header).WithBody(*block.Body())
}
