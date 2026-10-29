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
	"math"
	"math/big"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
)

type localSealRequest struct {
	chain   consensus.ChainHeaderReader
	block   *types.Block
	results chan<- *types.Block
	stop    <-chan struct{}
}

type localPoWTestEngine struct {
	consensus.Engine
	requests chan localSealRequest
	sealErr  error
}

func (e *localPoWTestEngine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	e.requests <- localSealRequest{chain, block, results, stop}
	return e.sealErr
}

func newLocalTestPoWMiner(t *testing.T, config PoWMinerConfig) (*PoWMiner, *powMinerTestShard, *localPoWTestEngine) {
	t.Helper()
	backend := &powMinerTestShard{tip: &types.Header{Number: big.NewInt(41), Difficulty: big.NewInt(5)}}
	engine := &localPoWTestEngine{Engine: ethash.NewFaker(), requests: make(chan localSealRequest, 8)}
	m, err := NewPoWMiner(backend, engine, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopLocalTestPoWMiner(m) })
	return m, backend, engine
}

func stopLocalTestPoWMiner(m *PoWMiner) {
	m.Disable()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}

func sealLocalTestWork(request localSealRequest, nonce uint64) {
	header := request.block.Header()
	header.Nonce, header.MixDigest = types.EncodeNonce(nonce), common.Hash{byte(nonce)}
	request.results <- request.block.WithSeal(header)
}

func TestPoWMinerLocalSealing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		coinbase := common.Address{1}
		m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{Coinbase: coinbase})
		adding := make(chan *types.Block, 1)
		backend.add = func(ctx context.Context, block *types.Block) error {
			adding <- block
			<-ctx.Done() // Model propagation that is slower than preparing the next work.
			return ctx.Err()
		}
		if _, _, err := m.GetWork(t.Context(), coinbase); err == nil {
			t.Fatal("GetWork accepted in local mode")
		}
		if _, err := m.SubmitWork(t.Context(), common.Hash{}, 0, common.Hash{}); err == nil {
			t.Fatal("SubmitWork accepted in local mode")
		}
		m.Start()
		m.Start()
		first := <-engine.requests
		if first.chain != backend || first.block.Coinbase() != coinbase || !m.Enabled() {
			t.Fatal("local sealing did not use the shard's chain view and coinbase")
		}
		sealLocalTestWork(first, 7)
		block := <-adding
		next := <-engine.requests
		if block.Nonce() != 7 || block.MixDigest() != (common.Hash{7}) || block.Transactions()[0].Hash() != first.block.Transactions()[0].Hash() {
			t.Fatal("sealed candidate was not passed to the shard layer")
		}
		select {
		case <-first.stop:
		default:
			t.Fatal("completed sealing request was not canceled")
		}
		stopLocalTestPoWMiner(m)
		select {
		case <-next.stop:
		default:
			t.Fatal("Disable did not cancel the next sealing task")
		}
		for _, wait := range backend.waits {
			if !wait {
				t.Fatal("local building did not request readiness waiting")
			}
		}
		if m.Enabled() {
			t.Fatal("Disable left mining enabled")
		}
	})
}

func TestPoWMinerLocalRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{})
		accepted := make(chan *types.Block, 1)
		backend.add = func(_ context.Context, block *types.Block) error {
			backend.setTip(block.Header())
			accepted <- block
			return nil
		}
		m.Start()
		old := <-engine.requests
		stopLocalTestPoWMiner(m)
		m.Start()
		current := <-engine.requests
		// A late response to the canceled session cannot reach the new session.
		sealLocalTestWork(old, 1)
		sealLocalTestWork(current, 2)
		block := <-accepted
		if block.Nonce() != 2 {
			t.Fatal("old session's result was accepted after restart")
		}
		stopLocalTestPoWMiner(m)
		if _, added := backend.counts(); added != 1 {
			t.Fatalf("accepted %d blocks after restart", added)
		}
	})
}

func TestPoWMinerLocalDisableDuringBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{})
		started := make(chan struct{})
		release := make(chan struct{})
		backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
			if binary.BigEndian.Uint64(block.Extra()) == 1 {
				close(started)
				<-release // Deliberately return a candidate after cancellation.
			}
			return block, nil
		}
		m.Start()
		<-started
		oldDone := m.done
		m.Disable()
		m.Start()
		close(release)
		request := <-engine.requests
		<-oldDone
		if binary.BigEndian.Uint64(request.block.Extra()) != 2 || !m.Enabled() {
			t.Fatal("old build started sealing or interfered with the restarted session")
		}
		stopLocalTestPoWMiner(m)
		if _, added := backend.counts(); added != 0 {
			t.Fatal("canceled build was accepted")
		}
		select {
		case <-engine.requests:
			t.Fatal("Disable allowed a late sealing request")
		default:
		}
	})
}

func TestPoWMinerLocalTaskFailure(t *testing.T) {
	for _, failure := range []string{"create error", "nil candidate", "seal error", "wrong result", "nil result"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{})
				switch failure {
				case "create error":
					backend.create = func(context.Context, *types.Block) (*types.Block, error) {
						return nil, errors.New("create failed")
					}
				case "nil candidate":
					backend.create = func(context.Context, *types.Block) (*types.Block, error) { return nil, nil }
				case "seal error":
					engine.sealErr = errors.New("seal failed")
				}
				m.Start()
				if failure == "seal error" || failure == "wrong result" || failure == "nil result" {
					request := <-engine.requests
					switch failure {
					case "wrong result":
						header := request.block.Header()
						header.Time++
						request.results <- request.block.WithSeal(header)
					case "nil result":
						request.results <- nil
					}
				}
				<-m.done
				if _, added := backend.counts(); added != 0 {
					t.Fatal("failed mining task reached the shard acceptance path")
				}
				backend.create, engine.sealErr = nil, nil
				m.Start()
				<-engine.requests
				stopLocalTestPoWMiner(m)
			})
		})
	}
}

func TestPoWMinerLocalSubmissionFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{})
		attempts := make(chan uint64, 2)
		backend.add = func(_ context.Context, block *types.Block) error {
			attempts <- block.Nonce()
			if block.Nonce() == 1 {
				return errors.New("propagation failed")
			}
			return nil
		}
		m.Start()
		first := <-engine.requests
		sealLocalTestWork(first, 1)
		if nonce := <-attempts; nonce != 1 {
			t.Fatalf("first submission nonce: %d", nonce)
		}
		next := <-engine.requests
		sealLocalTestWork(next, 2)
		if nonce := <-attempts; nonce != 2 {
			t.Fatalf("next submission nonce: %d", nonce)
		}
		stopLocalTestPoWMiner(m)
		if _, added := backend.counts(); added != 1 {
			t.Fatal("mining did not continue after propagation failed")
		}
	})
}

func TestPoWMinerSimulate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		coinbase := common.Address{1}
		m, backend, engine := newLocalTestPoWMiner(t, PoWMinerConfig{Simulate: true, Coinbase: coinbase})
		tip := backend.CurrentHeader()
		tip.Extra = []byte{1}
		newParent := tip.Hash()
		created := make(chan *types.Block, 3)
		backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
			if block.ParentHash() == newParent {
				block = block.WithBody(types.Body{Transactions: []*types.Transaction{types.NewTx(&types.LegacyTx{Nonce: 7})}})
			}
			created <- block
			return block, nil
		}
		accepted := make(chan *types.Block, 1)
		backend.add = func(_ context.Context, block *types.Block) error {
			accepted <- block
			return nil
		}
		m.Start()
		initial := <-created
		time.Sleep(99 * time.Millisecond)
		if builds, added := backend.counts(); builds != 1 || added != 0 {
			t.Fatal("simulation completed before its first 100ms check")
		}
		backend.setTip(tip)
		time.Sleep(time.Millisecond)
		block := <-accepted
		if block.ParentHash() != newParent || block.Hash() == initial.Hash() || block.Transactions()[0].Nonce() != 7 || block.Coinbase() != coinbase {
			t.Fatal("simulation did not rebuild from the latest shard state")
		}
		if block.Nonce() > math.MaxUint32 {
			t.Fatal("simulation did not use the reference's 32-bit nonce for the fresh block")
		}
		<-created // Fresh block for submission.
		<-created // Next mining task, started before propagation.
		stopLocalTestPoWMiner(m)
		select {
		case <-engine.requests:
			t.Fatal("simulation invoked the real nonce search")
		default:
		}
	})
}

func TestPoWMinerSimulateCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, backend, _ := newLocalTestPoWMiner(t, PoWMinerConfig{Simulate: true})
		created := make(chan struct{}, 1)
		backend.create = func(_ context.Context, block *types.Block) (*types.Block, error) {
			header := block.Header()
			header.Time += 10
			created <- struct{}{}
			return block.WithSeal(header), nil
		}
		m.Start()
		<-created
		time.Sleep(time.Second)
		stopLocalTestPoWMiner(m)
		if _, added := backend.counts(); added != 0 {
			t.Fatal("canceled simulation submitted a block")
		}
	})
}

func TestPoWMinerSimulationInterval(t *testing.T) {
	for _, test := range []struct {
		name      string
		gasUsed   uint64
		sample    float64
		blockTime time.Duration
		want      time.Duration
	}{
		{"empty", 0, 1, 2 * time.Second, 2 * time.Second},
		{"full", 100, 1, 2 * time.Second, 1200 * time.Millisecond},
		{"half, sample two", 50, 2, 2 * time.Second, 3200 * time.Millisecond},
		{"zero mean", 100, 10, 0, 0},
		{"large sample", 0, 10, math.MaxInt64, math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			block := types.NewBlockWithHeader(&types.Header{GasLimit: 100, GasUsed: test.gasUsed})
			delay, err := simulationInterval(block, test.blockTime, test.sample)
			if err != nil || delay != test.want {
				t.Fatalf("interval %v, %v; want %v", delay, err, test.want)
			}
		})
	}
	for _, header := range []*types.Header{{}, {GasLimit: 100, GasUsed: 101}} {
		if _, err := simulationInterval(types.NewBlockWithHeader(header), time.Second, 1); err == nil {
			t.Fatal("simulation accepted invalid gas usage")
		}
	}
	if _, err := NewPoWMiner(nil, nil, PoWMinerConfig{TargetBlockTime: -1}); err == nil {
		t.Fatal("accepted a negative target block time")
	}
}
