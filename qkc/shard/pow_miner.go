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
	workRefreshInterval time.Duration

	ctx      context.Context
	cancel   context.CancelFunc
	commands chan miningCommand
	tips     chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

type miningCommand struct {
	running   bool
	coinbases []common.Address // Non-nil replaces the address set, including an empty set
	done      chan struct{}
}

type miningTask struct {
	args    miner.QKCBlockArgs
	cancel  context.CancelFunc
	stop    <-chan struct{}
	block   *types.Block
	results chan *types.Block
	next    time.Time // Next build attempt, or refresh deadline for published work
}

type miningBuildResult struct {
	task  *miningTask
	block *types.Block
	err   error
}

type miningSealResult struct {
	task  *miningTask
	block *types.Block
}

// NewPoWMiner creates a paused miner initially scheduling work for coinbase.
// SetCoinbases changes the addresses to mine for. Each address has its own work
// refresh interval, independent of the builder's transaction-filling budget.
// Call Start after shard initialization. The engine must support simultaneous
// sealing tasks when multiple coinbases are configured.
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
		chain: chain, workRefreshInterval: workRefreshInterval,
		ctx: ctx, cancel: cancel, commands: make(chan miningCommand),
		tips: make(chan struct{}, 1), done: make(chan struct{}),
	}
	heads := make(chan core.ChainHeadEvent, 1)
	roots := make(chan struct{}, 1)
	headSub := chain.chain.SubscribeChainHeadEvent(heads)
	rootSub := chain.rootTipFeed.Subscribe(roots)
	m.wg.Add(1)
	go m.watchTips(heads, roots, headSub, rootSub)
	go m.loop(coinbase)
	return m, nil
}

// Start begins mining. Calling Start on a running miner is harmless.
func (m *PoWMiner) Start() error { return m.setRunning(true) }

// Resume starts a fresh task after a pause.
func (m *PoWMiner) Resume() error { return m.Start() }

// Pause cancels construction and sealing, and waits for any started import.
// Synchronization callers pause before syncing and resume after completion.
func (m *PoWMiner) Pause() error { return m.setRunning(false) }

// SetCoinbases replaces the addresses to mine for, preserving work for retained
// addresses and cancelling removed addresses. Duplicates are ignored; an empty
// list cancels all work. New addresses start building when the miner is running.
func (m *PoWMiner) SetCoinbases(coinbases []common.Address) error {
	return m.command(miningCommand{coinbases: append([]common.Address{}, coinbases...)})
}

// Stop permanently stops mining and waits for imports and construction to end.
// It is safe to call concurrently or more than once.
func (m *PoWMiner) Stop() error {
	m.cancel()
	<-m.done
	return nil
}

func (m *PoWMiner) setRunning(running bool) error {
	return m.command(miningCommand{running: running})
}

func (m *PoWMiner) command(cmd miningCommand) error {
	cmd.done = make(chan struct{})
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

func (m *PoWMiner) loop(coinbase common.Address) {
	var (
		running  bool
		building *miningTask
		tick     <-chan time.Time
		tasks    = make(map[common.Address]*miningTask)
		built    = make(chan miningBuildResult, 1)
		sealed   = make(chan miningSealResult)
		timer    = time.NewTimer(miningTimerInitialDelay)
	)
	timer.Stop()
	reset := func(coinbase common.Address, delay time.Duration) {
		if task := tasks[coinbase]; task != nil && task.cancel != nil {
			task.cancel()
		}
		tasks[coinbase] = &miningTask{
			args: miner.QKCBlockArgs{FeeRecipient: coinbase}, next: time.Now().Add(delay),
		}
	}
	resetAll := func() {
		for coinbase := range tasks {
			reset(coinbase, 0)
		}
	}
	reset(coinbase, 0)
	defer func() {
		m.cancel()
		resetAll()
		timer.Stop()
		m.wg.Wait()
		close(m.done)
	}()
	schedule := func() {
		timer.Stop()
		tick = nil
		if !running || m.ctx.Err() != nil {
			return
		}
		now := time.Now()
		var ready *miningTask
		for coinbase, task := range tasks {
			if task.block != nil && !task.next.After(now) {
				reset(coinbase, 0)
				task = tasks[coinbase]
			}
			if task.cancel == nil && !task.next.After(now) && (ready == nil || task.next.Before(ready.next)) {
				ready = task
			}
		}
		if ready != nil && building == nil {
			m.chain.mu.Lock()
			parent, root := m.chain.chain.CurrentBlock(), m.chain.rootTip
			m.chain.mu.Unlock()
			switch {
			case root == nil:
				ready.next = now.Add(miningRetryInterval)
			case parent.Time >= uint64(now.Unix()):
				// Low-difficulty sealing must not run the timestamp ahead of the clock.
				ready.next = time.Unix(int64(parent.Time)+1, 0)
			default:
				ctx, cancel := context.WithCancel(m.ctx)
				ready.args.Parent, ready.args.PrevRootBlock = parent.Hash(), root.Hash()
				ready.args.Timestamp = uint64(now.Unix())
				ready.cancel, ready.stop, ready.results = cancel, ctx.Done(), make(chan *types.Block, 1)
				building = ready
				m.wg.Add(1)
				go func(task *miningTask) {
					defer m.wg.Done()
					block, err := m.chain.BuildBlock(ctx, task.args)
					built <- miningBuildResult{task, block, err}
				}(ready)
			}
		}
		var next time.Time
		for _, task := range tasks {
			if task == building || (building != nil && task.cancel == nil && !task.next.After(now)) {
				continue
			}
			if next.IsZero() || task.next.Before(next) {
				next = task.next
			}
		}
		if !next.IsZero() {
			timer.Reset(time.Until(next))
			tick = timer.C
		}
	}
	engine := m.chain.chain.Engine()
	for {
		if m.ctx.Err() != nil {
			return
		}
		select {
		case cmd := <-m.commands:
			if cmd.coinbases != nil {
				keep := make(map[common.Address]bool, len(cmd.coinbases))
				for _, coinbase := range cmd.coinbases {
					keep[coinbase] = true
					if tasks[coinbase] == nil {
						reset(coinbase, 0)
					}
				}
				for coinbase, task := range tasks {
					if !keep[coinbase] {
						if task.cancel != nil {
							task.cancel()
						}
						delete(tasks, coinbase)
					}
				}
			} else if running != cmd.running {
				running = cmd.running
				resetAll()
			}
			schedule()
			close(cmd.done)
		case <-m.tips:
			for coinbase, task := range tasks {
				if !m.matchesTips(task.args) {
					reset(coinbase, 0)
				}
			}
			schedule()
		case <-tick:
			schedule()
		case result := <-built:
			building = nil
			task := result.task
			coinbase := task.args.FeeRecipient
			if tasks[coinbase] != task {
				schedule()
				continue
			}
			if result.err != nil {
				log.Warn("Failed to build shard candidate", "coinbase", coinbase, "err", result.err)
				reset(coinbase, miningRetryInterval)
				schedule()
				continue
			}
			if !m.matchesTips(task.args) {
				reset(coinbase, 0)
				schedule()
				continue
			}
			task.block = result.block
			if err := engine.Seal(m.chain.chain, task.block, task.results, task.stop); err != nil {
				log.Warn("Failed to seal shard candidate", "coinbase", coinbase, "err", err)
				reset(coinbase, miningRetryInterval)
				schedule()
				continue
			}
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				select {
				case block := <-task.results:
					select {
					case sealed <- miningSealResult{task, block}:
					case <-task.stop:
					}
				case <-task.stop:
				}
			}()
			// Construction has its own transaction-filling time budget. Start the
			// sealing interval now so slow builds still get a chance to publish work.
			task.next = time.Now().Add(m.workRefreshInterval)
			schedule()
		case result := <-sealed:
			task, block := result.task, result.block
			coinbase := task.args.FeeRecipient
			if tasks[coinbase] != task {
				continue
			}
			reset(coinbase, miningRetryInterval)
			if block == nil || block.Coinbase() != coinbase || engine.SealHash(block.Header()) != engine.SealHash(task.block.Header()) {
				log.Warn("Discarding unrelated shard sealing result")
			} else if err := m.importResult(task.args, block); err != nil {
				log.Warn("Failed to import mined shard block", "number", block.NumberU64(), "err", err)
			} else {
				resetAll()
			}
			schedule()
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *PoWMiner) matchesTips(args miner.QKCBlockArgs) bool {
	m.chain.mu.Lock()
	defer m.chain.mu.Unlock()
	return m.matchesTipsLocked(args)
}

func (m *PoWMiner) matchesTipsLocked(args miner.QKCBlockArgs) bool {
	return m.chain.rootTip != nil && m.chain.rootTip.Hash() == args.PrevRootBlock &&
		m.chain.chain.CurrentBlock().Hash() == args.Parent
}

func (m *PoWMiner) importResult(args miner.QKCBlockArgs, block *types.Block) error {
	m.chain.mu.Lock()
	defer m.chain.mu.Unlock()
	// Check freshness under the import lock so a concurrent tip update cannot
	// turn this result stale between the check and InsertShardBlock's shared path.
	if m.ctx.Err() != nil || !m.matchesTipsLocked(args) {
		return nil
	}
	return m.chain.insertShardBlock(block)
}
