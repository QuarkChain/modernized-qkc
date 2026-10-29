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
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type localMiningResult struct {
	block *types.Block
	err   error
}

func (m *PoWMiner) runLocal(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	var tasks sync.WaitGroup
	defer func() {
		cancel()
		tasks.Wait()
		m.mu.Lock()
		if m.done == done {
			m.cancel = nil
		}
		close(done)
		m.mu.Unlock()
	}()

	next := m.mineLocal(ctx, &tasks)
	for {
		var result localMiningResult
		select {
		case <-ctx.Done():
			return
		case result = <-next:
		}
		if ctx.Err() != nil {
			return
		}
		if result.err != nil {
			log.Warn("Local shard mining failed", "err", result.err)
			return
		}
		if result.block == nil {
			return
		}
		// Like the Python miner, begin the next task before propagating this
		// result. Each task has its own result channel and cancellation scope.
		next = m.mineLocal(ctx, &tasks)
		block := result.block
		if m.config.Simulate {
			// Simulation takes a fresh snapshot after waiting, so it includes
			// transactions and root references received during the interval.
			var err error
			block, err = m.createLocalBlock(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil || block == nil {
				log.Warn("Failed to rebuild simulated shard block", "err", err)
				continue
			}
			header := block.Header()
			header.Nonce = types.EncodeNonce(uint64(rand.Uint32()))
			block = block.WithSeal(header)
		}
		if ctx.Err() != nil {
			return
		}
		if err := m.backend.AddMinedBlock(ctx, block); err != nil && ctx.Err() == nil {
			log.Warn("Failed to accept locally mined shard block", "err", err)
		}
	}
}

// mineLocal starts one build-and-seal task. A buffered reply lets a finished task
// exit even while the shard layer is still propagating the preceding block.
func (m *PoWMiner) mineLocal(ctx context.Context, tasks *sync.WaitGroup) <-chan localMiningResult {
	reply := make(chan localMiningResult, 1)
	tasks.Add(1)
	go func() {
		defer tasks.Done()
		block, err := m.createLocalBlock(ctx)
		if err == nil && block != nil {
			block, err = m.sealLocalBlock(ctx, block)
		}
		reply <- localMiningResult{block, err}
	}()
	return reply
}

func (m *PoWMiner) createLocalBlock(ctx context.Context) (*types.Block, error) {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	block, err := m.backend.CreateBlockToMine(ctx, m.config.Coinbase, true)
	if err != nil {
		return nil, fmt.Errorf("create mining block: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return block, nil
}

func (m *PoWMiner) sealLocalBlock(ctx context.Context, block *types.Block) (*types.Block, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.config.Simulate {
		return m.simulateBlock(ctx, block)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	results := make(chan *types.Block, 1)
	if err := m.engine.Seal(m.backend, block, results, ctx.Done()); err != nil {
		return nil, fmt.Errorf("seal mining block: %w", err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case sealed := <-results:
		if sealed == nil || m.engine.SealHash(sealed.Header()) != m.engine.SealHash(block.Header()) {
			return nil, errors.New("engine returned a result for different mining work")
		}
		return copyMiningBlock(block, sealed.Header()), nil
	}
}

func (m *PoWMiner) simulateBlock(ctx context.Context, block *types.Block) (*types.Block, error) {
	delay, err := simulationInterval(block, m.config.TargetBlockTime, rand.ExpFloat64())
	if err != nil {
		return nil, err
	}
	target := time.Unix(int64(block.Time()), 0).Add(delay)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(target) {
				header := block.Header()
				header.Nonce, header.MixDigest = types.EncodeNonce(rand.Uint64()), [32]byte{}
				return block.WithSeal(header), nil
			}
		}
	}
}

// simulationInterval scales a unit-mean exponential sample using the Python
// miner's shard gas usage adjustment.
func simulationInterval(block *types.Block, targetBlockTime time.Duration, sample float64) (time.Duration, error) {
	if block.GasLimit() == 0 || block.GasUsed() > block.GasLimit() {
		return 0, errors.New("invalid gas usage for mining simulation")
	}
	// Python samples an exponential interval and reduces its mean by up to
	// 40% for a full shard block to compensate for transaction execution time.
	gasRatio := float64(block.GasUsed()) / float64(block.GasLimit())
	interval := sample * float64(targetBlockTime) * (1 - gasRatio*0.4)
	// Avoid wrapping a very large sample into a negative duration.
	delay := time.Duration(math.MaxInt64)
	if interval < float64(math.MaxInt64) {
		delay = time.Duration(interval)
	}
	return delay, nil
}
