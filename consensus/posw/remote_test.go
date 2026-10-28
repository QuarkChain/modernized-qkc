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
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func remoteTestBlock(number uint64) *types.Block {
	return types.NewBlockWithHeader(&types.Header{
		Number: new(big.Int).SetUint64(number), Difficulty: new(big.Int),
		MixDigest: common.BigToHash(big.NewInt(100)),
		Extra:     (&types.QKCExtra{ShardID: 1}).Encode(),
	})
}

func remoteTestVerify(*types.Header) error { return nil }

func TestRemoteSealerWork(t *testing.T) {
	s := new(remoteSealer)
	if _, err := s.GetWork(); !errors.Is(err, errNoMiningWork) {
		t.Fatalf("initial work error %v", err)
	}
	block := remoteTestBlock(30000)
	results := make(chan *types.Block, 1)
	// The supplied effective difficulty differs from the header's base difficulty.
	if err := s.publish(block, results, nil, big.NewInt(2), remoteTestVerify); err != nil {
		t.Fatal(err)
	}
	hash, _ := block.Header().QKCSealHash()
	want := [4]string{
		hash.Hex(), common.BytesToHash(crypto.Keccak256(make([]byte, 32))).Hex(),
		"0x8" + strings.Repeat("0", 63), "0x7530",
	}
	if work, err := s.GetWork(); err != nil || work != want {
		t.Fatalf("work %v, error %v, want %v", work, err, want)
	}
	if err := s.publish(block, results, nil, big.NewInt(1), remoteTestVerify); err != nil {
		t.Fatal(err)
	}
	if work, err := s.GetWork(); err != nil || work[2] != "0x"+strings.Repeat("f", 64) {
		t.Fatalf("difficulty-one work %v, error %v", work, err)
	}
}

func TestRemoteSealerSubmission(t *testing.T) {
	var (
		s       remoteSealer
		block   = remoteTestBlock(1)
		results = make(chan *types.Block, 1)
		nonce   = types.EncodeNonce(0x0102030405060708)
		digest  = common.Hash{0x42}
		calls   int
	)
	hash, _ := block.Header().QKCSealHash()
	verify := func(header *types.Header) error {
		calls++
		extra, _ := header.QKCExtra()
		if extra.Nonce != nonce || extra.MixHash != digest {
			return errors.New("invalid seal")
		}
		return nil
	}
	if err := s.publish(block, results, nil, big.NewInt(2), verify); err != nil {
		t.Fatal(err)
	}
	if s.SubmitWork(nonce, common.Hash{0xff}, digest) || calls != 0 {
		t.Fatal("unknown work reached verification")
	}
	if s.SubmitWork(nonce, hash, common.Hash{}) || len(results) != 0 {
		t.Fatal("invalid proof was accepted")
	}
	if !s.SubmitWork(nonce, hash, digest) {
		t.Fatal("valid proof was rejected")
	}
	sealed := <-results
	extra, _ := sealed.Header().QKCExtra()
	if extra.Nonce != nonce || extra.MixHash != digest {
		t.Fatalf("unexpected seal %+v", extra)
	}
	if sealed.Nonce() != 0 || sealed.MixDigest() != block.MixDigest() || sealed.Difficulty().Sign() != 0 {
		t.Fatal("submission changed the reserved header fields")
	}
	if got, _ := sealed.Header().QKCSealHash(); got != hash {
		t.Fatal("submission changed the seal hash")
	}
	if original, _ := block.Header().QKCExtra(); original.Nonce != (types.BlockNonce{}) || original.MixHash != (common.Hash{}) {
		t.Fatal("submission modified the candidate")
	}
	if s.SubmitWork(nonce, hash, digest) || calls != 2 {
		t.Fatal("duplicate submission was verified or accepted")
	}
	if _, err := s.GetWork(); !errors.Is(err, errNoMiningWork) {
		t.Fatalf("accepted task is still available: %v", err)
	}
}

func TestRemoteSealerCancellation(t *testing.T) {
	for _, action := range []string{"cancel", "replace", "close"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var s remoteSealer
				block := remoteTestBlock(1)
				results := make(chan *types.Block, 1)
				stop, started, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				verify := func(*types.Header) error {
					close(started)
					<-release
					return nil
				}
				if err := s.publish(block, results, stop, big.NewInt(2), verify); err != nil {
					t.Fatal(err)
				}
				hash, _ := block.Header().QKCSealHash()
				submitted := make(chan bool, 1)
				go func() { submitted <- s.SubmitWork(types.BlockNonce{}, hash, common.Hash{}) }()
				<-started

				var closed chan struct{}
				switch action {
				case "cancel":
					close(stop)
				case "replace":
					next := remoteTestBlock(2)
					if err := s.publish(next, results, nil, big.NewInt(2), remoteTestVerify); err != nil {
						t.Fatal(err)
					}
					nextHash, _ := next.Header().QKCSealHash()
					if work, err := s.GetWork(); err != nil || work[0] != nextHash.Hex() {
						t.Fatalf("replacement work %v, error %v", work, err)
					}
				case "close":
					closed = make(chan struct{})
					go func() { s.Close(); close(closed) }()
					synctest.Wait()
					select {
					case <-closed:
						t.Fatal("Close did not wait for verification")
					default:
					}
					if _, err := s.GetWork(); !errors.Is(err, errRemoteSealerStopped) {
						t.Fatalf("closed sealer work error %v", err)
					}
				}
				close(release)
				if <-submitted || len(results) != 0 {
					t.Fatal("obsolete verification delivered a block")
				}
				if action == "cancel" {
					if _, err := s.GetWork(); !errors.Is(err, errNoMiningWork) {
						t.Fatalf("cancelled work is available: %v", err)
					}
				}
				if closed != nil {
					<-closed
				}
				if s.SubmitWork(types.BlockNonce{}, hash, common.Hash{}) {
					t.Fatal("obsolete submission was accepted")
				}
				s.Close()
				if err := s.publish(block, results, nil, big.NewInt(2), remoteTestVerify); !errors.Is(err, errRemoteSealerStopped) {
					t.Fatalf("publish after Close: %v", err)
				}
			})
		})
	}
}

func TestRemoteSealerCancelledPublish(t *testing.T) {
	var s remoteSealer
	block := remoteTestBlock(1)
	results := make(chan *types.Block, 1)
	if err := s.publish(block, results, nil, big.NewInt(2), remoteTestVerify); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	if err := s.publish(remoteTestBlock(2), results, stop, big.NewInt(2), remoteTestVerify); err != nil {
		t.Fatal(err)
	}
	hash, _ := block.Header().QKCSealHash()
	if work, err := s.GetWork(); err != nil || work[0] != hash.Hex() {
		t.Fatalf("cancelled publication replaced current work: %v, %v", work, err)
	}
}

func TestRemoteSealerConcurrentSubmissions(t *testing.T) {
	var s remoteSealer
	block := remoteTestBlock(1)
	results := make(chan *types.Block, 8)
	started, release := make(chan struct{}, 8), make(chan struct{})
	verify := func(*types.Header) error {
		started <- struct{}{}
		<-release
		return nil
	}
	if err := s.publish(block, results, nil, big.NewInt(2), verify); err != nil {
		t.Fatal(err)
	}
	hash, _ := block.Header().QKCSealHash()
	var accepted atomic.Int32
	done := make(chan struct{}, 8)
	for range 8 {
		go func() {
			if s.SubmitWork(types.BlockNonce{}, hash, common.Hash{}) {
				accepted.Add(1)
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-started
	}
	close(release)
	for range 8 {
		<-done
	}
	if accepted.Load() != 1 || len(results) != 1 {
		t.Fatalf("accepted %d submissions and delivered %d blocks", accepted.Load(), len(results))
	}
}

func TestRemoteSealerBusyResults(t *testing.T) {
	var s remoteSealer
	block := remoteTestBlock(1)
	results := make(chan *types.Block, 1)
	results <- block
	if err := s.publish(block, results, nil, big.NewInt(2), remoteTestVerify); err != nil {
		t.Fatal(err)
	}
	hash, _ := block.Header().QKCSealHash()
	if s.SubmitWork(types.BlockNonce{}, hash, common.Hash{}) {
		t.Fatal("submission succeeded with a full result channel")
	}
	<-results
	if !s.SubmitWork(types.BlockNonce{}, hash, common.Hash{}) {
		t.Fatal("rejected submission consumed the task")
	}
}
