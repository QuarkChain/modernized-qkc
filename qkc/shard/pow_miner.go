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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
)

const (
	// miningRetryInterval backs off failed attempts and polls shard initialization.
	// One second avoids a tight retry loop while allowing prompt recovery.
	miningRetryInterval = time.Second

	// miningTimerInitialDelay is arbitrary: the timer is stopped immediately after
	// creation. schedule sets the actual delay with Reset before the timer is used.
	miningTimerInitialDelay = time.Hour
)

// ErrPoWMinerStopped is returned when controlling a miner after Stop.
var ErrPoWMinerStopped = errors.New("pow miner stopped")

// PoWMiner schedules candidates, sealing and imports using the builder and engine
// bound to ShardChain. The caller owns those dependencies and stops them after Stop.
type PoWMiner struct {
	chain               *ShardChain
	coinbase            common.Address
	workRefreshInterval time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	commands chan miningCommand
	tips     chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

type miningCommand struct {
	running bool
	done    chan struct{}
}

type miningTask struct {
	args    miner.QKCBlockArgs
	cancel  context.CancelFunc
	stop    <-chan struct{}
	block   *types.Block
	results chan *types.Block
}

type miningBuildResult struct {
	task  *miningTask
	block *types.Block
	err   error
}

// NewPoWMiner creates a paused miner with an explicit coinbase and work refresh
// interval, independent of the builder's transaction-filling budget.
// Call Start after shard initialization.
func NewPoWMiner(chain *ShardChain, coinbase common.Address, workRefreshInterval time.Duration) (*PoWMiner, error) {
	if chain == nil || chain.builder == nil {
		return nil, errors.New("shard chain and builder must be configured")
	}
	if chain.chain.Config().QuarkChain == nil {
		return nil, errNotQuarkChain
	}
	if workRefreshInterval <= 0 {
		return nil, errors.New("mining work refresh interval must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &PoWMiner{
		chain: chain, coinbase: coinbase, workRefreshInterval: workRefreshInterval,
		ctx: ctx, cancel: cancel, commands: make(chan miningCommand),
		tips: make(chan struct{}, 1), done: make(chan struct{}),
	}
	heads := make(chan core.ChainHeadEvent, 1)
	roots := make(chan struct{}, 1)
	headSub := chain.chain.SubscribeChainHeadEvent(heads)
	rootSub := chain.rootTipFeed.Subscribe(roots)
	m.wg.Add(1)
	go m.watchTips(heads, roots, headSub, rootSub)
	go m.loop()
	return m, nil
}

// Start begins mining. Calling Start on a running miner is harmless.
func (m *PoWMiner) Start() error { return m.setRunning(true) }

// Resume starts a fresh task after a pause.
func (m *PoWMiner) Resume() error { return m.Start() }

// Pause cancels construction and sealing, and waits for any started import.
// Synchronization callers pause before syncing and resume after completion.
func (m *PoWMiner) Pause() error { return m.setRunning(false) }

// Stop permanently stops mining and waits for imports and construction to end.
// It is safe to call concurrently or more than once.
func (m *PoWMiner) Stop() error {
	m.cancel()
	<-m.done
	return nil
}

func (m *PoWMiner) setRunning(running bool) error {
	cmd := miningCommand{running: running, done: make(chan struct{})}
	select {
	case m.commands <- cmd:
	case <-m.ctx.Done():
		return ErrPoWMinerStopped
	}
	select {
	case <-cmd.done:
		return nil
	case <-m.ctx.Done():
		return ErrPoWMinerStopped
	}
}

// Drain chain events independently of imports: SetCanonical sends head events
// synchronously, including while the mining loop is importing its own result.
func (m *PoWMiner) watchTips(heads <-chan core.ChainHeadEvent, roots <-chan struct{}, headSub, rootSub event.Subscription) {
	defer m.wg.Done()
	defer headSub.Unsubscribe()
	defer rootSub.Unsubscribe()
	for {
		select {
		case <-heads:
		case <-roots:
		case <-headSub.Err():
			// The chain can close headSub; rootSub is only unsubscribed on exit.
			m.cancel()
			return
		case <-m.ctx.Done():
			return
		}
		select {
		case m.tips <- struct{}{}:
		default:
		}
	}
}

func (m *PoWMiner) loop() {
	var (
		running  bool
		building bool
		task     *miningTask
		results  <-chan *types.Block
		tick     <-chan time.Time
		built    = make(chan miningBuildResult, 1)
		timer    = time.NewTimer(miningTimerInitialDelay)
	)
	timer.Stop()
	cancelTask := func() {
		if task != nil {
			task.cancel()
			task = nil
		}
		results = nil
	}
	defer func() {
		m.cancel()
		cancelTask()
		timer.Stop()
		m.wg.Wait()
		close(m.done)
	}()
	schedule := func(delay time.Duration) {
		timer.Reset(delay)
		tick = timer.C
	}
	start := func() {
		cancelTask()
		timer.Stop()
		tick = nil
		if !running || building || m.ctx.Err() != nil {
			return
		}
		m.chain.mu.Lock()
		parent, root := m.chain.chain.CurrentBlock(), m.chain.rootTip
		m.chain.mu.Unlock()
		if root == nil {
			schedule(miningRetryInterval)
			return
		}
		now := time.Now()
		if parent.Time >= uint64(now.Unix()) {
			// Low-difficulty sealing must not run the timestamp ahead of the clock.
			schedule(time.Until(time.Unix(int64(parent.Time)+1, 0)))
			return
		}
		ctx, cancel := context.WithCancel(m.ctx)
		task = &miningTask{
			args: miner.QKCBlockArgs{
				Parent: parent.Hash(), PrevRootBlock: root.Hash(),
				Timestamp: uint64(now.Unix()), FeeRecipient: m.coinbase,
			},
			cancel: cancel, stop: ctx.Done(), results: make(chan *types.Block, 1),
		}
		building = true
		m.wg.Add(1)
		go func(task *miningTask) {
			defer m.wg.Done()
			block, err := m.chain.BuildBlock(ctx, task.args)
			built <- miningBuildResult{task, block, err}
		}(task)
	}
	engine := m.chain.chain.Engine()
	for {
		if m.ctx.Err() != nil {
			return
		}
		select {
		case cmd := <-m.commands:
			if running != cmd.running {
				running = cmd.running
				start()
			}
			close(cmd.done)
		case <-m.tips:
			if task == nil || !m.matchesHead(task.args) {
				start()
			}
		case <-tick:
			start()
		case result := <-built:
			building = false
			if result.task != task {
				start()
				continue
			}
			if result.err != nil {
				log.Warn("Failed to build shard candidate", "err", result.err)
				cancelTask()
				schedule(miningRetryInterval)
				continue
			}
			if !m.matchesHead(task.args) {
				start()
				continue
			}
			task.block = result.block
			if err := engine.Seal(m.chain.chain, task.block, task.results, task.stop); err != nil {
				log.Warn("Failed to seal shard candidate", "err", err)
				cancelTask()
				schedule(miningRetryInterval)
				continue
			}
			results = task.results
			// Construction has its own transaction-filling time budget. Start the
			// sealing interval now so slow builds still get a chance to publish work.
			schedule(m.workRefreshInterval)
		case block := <-results:
			current := task
			cancelTask()
			if block == nil || engine.SealHash(block.Header()) != engine.SealHash(current.block.Header()) {
				log.Warn("Discarding unrelated shard sealing result")
				schedule(miningRetryInterval)
			} else if err := m.importResult(current.args, block); err != nil {
				log.Warn("Failed to import mined shard block", "number", block.NumberU64(), "err", err)
				schedule(miningRetryInterval)
			} else {
				start()
			}
		case <-m.ctx.Done():
			return
		}
	}
}

// Root validity is checked on import; advancing the root tip alone does not
// invalidate work that still extends the shard head.
func (m *PoWMiner) matchesHead(args miner.QKCBlockArgs) bool {
	return m.chain.chain.CurrentBlock().Hash() == args.Parent
}

func (m *PoWMiner) importResult(args miner.QKCBlockArgs, block *types.Block) error {
	m.chain.mu.Lock()
	defer m.chain.mu.Unlock()
	// Check freshness under the import lock so a concurrent head update cannot
	// turn this result stale between the check and InsertShardBlock's shared path.
	if m.ctx.Err() != nil || !m.matchesHead(args) {
		return nil
	}
	return m.chain.insertShardBlock(block)
}
