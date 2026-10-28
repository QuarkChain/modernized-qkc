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

package posw

import (
	"errors"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	errNoMiningWork        = errors.New("no mining work available yet")
	errRemoteSealerStopped = errors.New("remote sealer stopped")
)

// remoteSealer manages one shard's current remote task. The engine supplies the
// effective difficulty and its seal verifier when publishing a candidate. The
// zero value is ready to use and starts without mining work.
type remoteSealer struct {
	mu      sync.Mutex
	current *remoteTask
	closed  bool
	wg      sync.WaitGroup // Seal verifications that must finish before Close returns
}

type remoteTask struct {
	block   *types.Block
	work    [4]string
	results chan<- *types.Block
	stop    <-chan struct{}
	verify  func(*types.Header) error
}

// publish replaces the current task. The caller computes difficulty and verify
// using the same engine and parent state as block verification. Proof verification
// is mandatory; the remote sealer has no option to bypass it.
func (s *remoteSealer) publish(block *types.Block, results chan<- *types.Block, stop <-chan struct{}, difficulty *big.Int, verify func(*types.Header) error) error {
	if block == nil || results == nil || verify == nil || difficulty == nil || difficulty.Sign() <= 0 || difficulty.BitLen() > 256 {
		return errors.New("invalid remote mining task")
	}
	header := block.Header()
	if header.Number == nil || !header.Number.IsUint64() || header.Number.Sign() == 0 {
		return errors.New("invalid remote mining block number")
	}
	hash, err := header.QKCSealHash()
	if err != nil {
		return err
	}
	target := new(big.Int).Div(new(big.Int).Lsh(big.NewInt(1), 256), difficulty)
	if target.BitLen() > 256 {
		// At difficulty one every 256-bit result is valid. Truncating 2^256
		// to a hash would advertise a zero target instead.
		target.Sub(target, big.NewInt(1))
	}
	task := &remoteTask{
		block: block, results: results, stop: stop, verify: verify,
		work: [4]string{
			hash.Hex(), common.BytesToHash(seedHash(header.Number.Uint64())).Hex(),
			common.BytesToHash(target.Bytes()).Hex(), hexutil.EncodeBig(header.Number),
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errRemoteSealerStopped
	}
	if remoteTaskStopped(task) {
		return nil
	}
	s.current = task
	return nil
}

func (s *remoteSealer) GetWork() ([4]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return [4]string{}, errRemoteSealerStopped
	}
	if s.current == nil || remoteTaskStopped(s.current) {
		s.current = nil
		return [4]string{}, errNoMiningWork
	}
	return s.current.work, nil
}

func (s *remoteSealer) SubmitWork(nonce types.BlockNonce, hash, digest common.Hash) bool {
	s.mu.Lock()
	task := s.current
	if s.closed || task == nil || remoteTaskStopped(task) || task.work[0] != hash.Hex() {
		s.mu.Unlock()
		return false
	}
	// Close prevents further Add calls under mu before waiting for verifiers.
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	header := task.block.Header()
	extra, err := header.QKCExtra()
	if err != nil {
		return false
	}
	extra.Nonce, extra.MixHash = nonce, digest
	header.Extra = extra.Encode()
	if err := task.verify(header); err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.current != task || remoteTaskStopped(task) {
		return false
	}
	select {
	case task.results <- task.block.WithSeal(header):
		// Only a delivered result consumes the task. A busy result channel
		// rejects this submission but leaves the task available for a retry.
		s.current = nil
		return true
	default:
		return false
	}
}

// Close invalidates work immediately and waits for in-flight verification. The
// engine closes its verification resources after this returns.
func (s *remoteSealer) Close() error {
	s.mu.Lock()
	s.closed = true
	s.current = nil
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

func remoteTaskStopped(task *remoteTask) bool {
	select {
	case <-task.stop:
		return true
	default:
		return false
	}
}
