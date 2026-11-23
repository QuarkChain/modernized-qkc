// Copyright 2026-2027 The quarkchain Authors
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

package root

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// Consensus rules are tested in poswroot. This engine controls import failures
// and verifies that consensus can query the reader while insertion holds its lock.
type insertEngine struct {
	posw.RootEngine
	calls int
	err   error
}

func (e *insertEngine) VerifyBlock(chain posw.RootChainReader, block *types.RootBlock) error {
	e.calls++
	if e.err != nil {
		return e.err
	}
	_, err := chain.GetRootHeader(block.ParentHash())
	return err
}

func newInsertEnv(t *testing.T) (*rootEnv, *insertEngine) {
	t.Helper()
	f := newRootEnv(t)
	e := &insertEngine{RootEngine: f.engine}
	f.engine, f.chain.engine = e, e
	if err := f.chain.Init(); err != nil {
		t.Fatal(err)
	}
	return f, e
}

func TestRootInsertForkChoiceAndConfirmations(t *testing.T) {
	f, engine := newInsertEnv(t)
	shards := types.ShardBlockHeaders{}
	for _, number := range []int64{1, 2} {
		shards = append(shards, &coretypes.Header{
			Number: big.NewInt(number), Difficulty: big.NewInt(1),
			Extra: (&coretypes.QKCExtra{ChainID: big.NewInt(10001)}).Encode(),
		})
	}
	a := types.NewRootBlock(f.block(f.genesis, 1).Header(), shards, nil)
	b := f.block(a, 2)
	lower := f.block(f.genesis, 3)
	equal := f.block(a, 4)
	fork := f.block(equal, 5)
	for _, test := range []struct {
		block, tip *types.RootBlock
	}{
		{a, a}, {b, b}, {lower, b}, {equal, b}, {fork, fork},
	} {
		f.client.onBroadcast = func(_ context.Context, block *types.RootBlock) error {
			// Delivery must observe the complete local commit and the new memory tip.
			if f.chain.RootTip().Hash() != test.tip.Hash() || qkcrawdb.ReadRootBlock(f.db, block.Hash()) == nil {
				t.Fatal("broadcast preceded local commit")
			}
			if _, err := f.chain.GetLastConfirmedShardBlocks(block.Hash()); err != nil {
				t.Fatal(err)
			}
			if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != block.Hash() {
				t.Fatal("broadcast lacks its atomically committed recovery marker", hash, err)
			}
			return nil
		}
		if err := f.chain.InsertBlock(test.block); err != nil {
			t.Fatal(err)
		}
		if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != test.tip.Hash() {
			t.Fatal("wrong persisted tip", hash, err)
		}
		if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != (common.Hash{}) {
			t.Fatal("successful broadcast retained its recovery marker", hash, err)
		}
	}
	if engine.calls != 5 || len(f.client.broadcasts) != 5 {
		t.Fatal("verification or side-chain delivery was skipped")
	}
	for number, block := range []*types.RootBlock{f.genesis, a, equal, fork} {
		if hash := qkcrawdb.ReadRootCanonicalHash(f.db, uint32(number)); hash != block.Hash() {
			t.Fatal("fork canonical index was not rewritten", number, hash)
		}
	}
	for _, block := range []*types.RootBlock{a, b, equal, fork, lower, f.genesis} {
		confirmed, err := f.chain.GetLastConfirmedShardBlocks(block.Hash())
		want := f.confirmed["10001"]
		if block != lower && block != f.genesis {
			want = shards[1].Hash()
		}
		if err != nil || confirmed["10001"] != want || confirmed["10002"] != f.confirmed["10002"] {
			t.Fatal("confirmations did not follow the parent branch", confirmed, err)
		}
	}
}

func TestRootInsertShorterHeavierBranchAfterRestart(t *testing.T) {
	f, _ := newInsertEnv(t)
	parent := f.genesis
	for seed := byte(1); seed <= 3; seed++ {
		parent = f.block(parent, seed)
		if err := f.chain.InsertBlock(parent); err != nil {
			t.Fatal(err)
		}
	}
	f.chain = f.open()
	if f.chain.RootTip().Number != 3 {
		t.Fatal("restart did not recover the persisted tip")
	}
	if err := f.chain.Init(); err != nil {
		t.Fatal(err)
	}
	header := f.block(f.genesis, 9).Header()
	header.Difficulty, header.TotalDifficulty = *uint256.NewInt(500), *uint256.NewInt(600)
	heavy := types.NewRootBlock(header, nil, nil)
	// Failure while staging deletions must leave even the higher persisted index.
	failure := errors.New("delete index failed")
	f.db.batchDeleteErr = failure
	if err := f.chain.InsertBlock(heavy); !errors.Is(err, failure) {
		t.Fatal("index deletion error lost", err)
	}
	if hash := qkcrawdb.ReadRootCanonicalHash(f.db, 3); hash != parent.Hash() || qkcrawdb.HasRootBlock(f.db, heavy.Hash()) {
		t.Fatal("failed reorg partially committed", hash)
	}
	f.db.batchDeleteErr = nil
	if err := f.chain.InsertBlock(heavy); err != nil {
		t.Fatal(err)
	}
	if hash := qkcrawdb.ReadRootCanonicalHash(f.db, 1); hash != heavy.Hash() || f.chain.RootTip().Hash() != heavy.Hash() {
		t.Fatal("shorter heavier branch did not become canonical", hash)
	}
	for _, number := range []uint32{2, 3} {
		if hash := qkcrawdb.ReadRootCanonicalHash(f.db, number); hash != (common.Hash{}) {
			t.Fatal("stale canonical index above new tip", number, hash)
		}
	}
	if !qkcrawdb.HasRootBlock(f.db, parent.Hash()) {
		t.Fatal("reorg removed the old branch block")
	}
}

func TestRootInsertKnownBlockAfterRestartUsesStoredBody(t *testing.T) {
	f, engine := newInsertEnv(t)
	block := f.block(f.genesis, 1).WithBody(nil, []byte("stored body"))
	f.client.broadcastErr = errors.New("delivery interrupted")
	if err := f.chain.InsertBlock(block); err == nil {
		t.Fatal("delivery failure accepted")
	}
	f.client.broadcastErr = nil
	f.chain = f.open()
	if err := f.chain.Init(); err != nil {
		t.Fatal(err)
	}
	if f.chain.RootTip().Hash() != block.Hash() {
		t.Fatal("Init changed the persisted tip")
	}
	engine.err = errors.New("known blocks must not be reverified")
	f.client.onBroadcast = func(_ context.Context, delivered *types.RootBlock) error {
		if string(delivered.TrackingData()) != "stored body" || len(delivered.ShardBlockHeaders()) != 0 {
			t.Fatal("used the supplied body instead of the verified stored body")
		}
		return nil
	}
	// Same header hash, unrelated body: the caller's body must be ignored.
	input := block.WithBody(f.genesis.ShardBlockHeaders(), []byte("untrusted body"))
	if err := f.chain.InsertBlock(input); err != nil {
		t.Fatal(err)
	}
	if engine.calls != 1 || f.chain.RootTip().Hash() != block.Hash() {
		t.Fatal("known block changed runtime tip or was reverified")
	}
	if !reflect.DeepEqual(qkcrawdb.ReadRootBlock(f.db, block.Hash()).TrackingData(), block.TrackingData()) {
		t.Fatal("known block was overwritten")
	}
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, block.Hash(), nil); err != nil {
		t.Fatal(err)
	}
	before := len(f.client.broadcasts)
	if err := f.chain.InsertBlock(input); err == nil || len(f.client.broadcasts) != before {
		t.Fatal("known block with incomplete confirmations was delivered", err)
	}
}

func TestRootInsertStaleHeightLimit(t *testing.T) {
	for _, test := range []struct {
		name   string
		tip    uint32
		stored bool
		reject bool
	}{
		{"too old", 22502, false, true},
		{"known but too old", 22502, true, true},
		{"at limit", 22501, false, false},
		{"ahead of tip", 0, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, engine := newInsertEnv(t)
			block := f.block(f.genesis, 1)
			if test.stored {
				f.store(block, false)
			}
			if test.tip != 0 {
				// Only the runtime tip's height affects admission. Keep its TD equal
				// to the candidate's so accepted side blocks do not change canonicality.
				tip := f.block(f.genesis, 2).Header()
				tip.Number = test.tip
				f.chain.tip.Store(tip)
			}
			before := f.chain.RootTip().Hash()
			err := f.chain.InsertBlock(block)
			if !test.reject {
				if err != nil || engine.calls != 1 || len(f.client.broadcasts) != 1 {
					t.Fatal("admissible block was not verified and delivered", err)
				}
				return
			}
			if err == nil || engine.calls != 0 || len(f.client.broadcasts) != 0 || !f.chain.Ready() {
				t.Fatal("stale block was processed or disabled the chain", err)
			}
			if f.chain.RootTip().Hash() != before || qkcrawdb.HasRootBlock(f.db, block.Hash()) != test.stored {
				t.Fatal("rejected stale block changed chain state")
			}
			if !test.stored {
				if _, err := f.chain.GetLastConfirmedShardBlocks(block.Hash()); err == nil {
					t.Fatal("rejected stale block left confirmations")
				}
			}
		})
	}
}

func TestRootInsertLocalFailureAndRetry(t *testing.T) {
	for _, stage := range []string{"verify", "read", "put", "committing marker", "commit"} {
		t.Run(stage, func(t *testing.T) {
			f, engine := newInsertEnv(t)
			block := f.block(f.genesis, 1)
			failure := errors.New("import failed")
			switch stage {
			case "verify":
				engine.err = failure
			case "read":
				f.db.readErr = failure
			case "put":
				f.db.putErr = failure
			case "committing marker":
				f.db.putErr, f.db.putKey = failure, "qkc_root_committing"
			case "commit":
				f.db.batchErr = failure
			}
			if err := f.chain.InsertBlock(block); !errors.Is(err, failure) {
				t.Fatal("import error lost", err)
			}
			if !f.chain.Ready() {
				t.Fatal("local failure disabled the chain")
			}
			engine.err, f.db.readErr, f.db.putErr, f.db.batchErr = nil, nil, nil, nil
			if f.chain.RootTip().Hash() != f.genesis.Hash() || len(f.client.broadcasts) != 0 || qkcrawdb.HasRootBlock(f.db, block.Hash()) {
				t.Fatal("failed import changed the tip, stored the block or broadcast")
			}
			if _, err := qkcrawdb.ReadRootConfirmedShardBlocks(f.db, block.Hash()); err == nil {
				t.Fatal("failed import left confirmations")
			}
			if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != f.genesis.Hash() {
				t.Fatal("failed import changed canonical storage", hash, err)
			}
			if hash := qkcrawdb.ReadRootCanonicalHash(f.db, 1); hash != (common.Hash{}) {
				t.Fatal("failed import left a canonical index", hash)
			}
			if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != (common.Hash{}) {
				t.Fatal("failed local commit left a recovery marker", hash, err)
			}
			if err := f.chain.InsertBlock(block); err != nil {
				t.Fatal("import retry failed", err)
			}
		})
	}
}

func TestRootInsertRepairsUnavailableBlock(t *testing.T) {
	for _, stage := range []string{"read error", "decode error", "hash mismatch", "verify failure", "commit failure"} {
		t.Run(stage, func(t *testing.T) {
			f, engine := newInsertEnv(t)
			block := f.block(f.genesis, 1).WithBody(nil, []byte("replacement"))
			f.store(block.WithBody(nil, []byte("old body")), false)
			oldConfirmed := map[string]common.Hash{"10001": {88}, "10002": {99}}
			if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, block.Hash(), oldConfirmed); err != nil {
				t.Fatal(err)
			}
			// Fail only the target block's read; parent data remains available
			// for full verification and reconstruction of the confirmation snapshot.
			f.db.nextRead = func() ([]byte, error) { return nil, errors.New("block read failed") }
			failure := errors.New("repair failed")
			switch stage {
			case "decode error":
				f.db.nextRead = func() ([]byte, error) { return []byte{0xff}, nil }
			case "hash mismatch":
				data, err := f.block(f.genesis, 2).MarshalSSZ()
				if err != nil {
					t.Fatal(err)
				}
				f.db.nextRead = func() ([]byte, error) { return data, nil }
			case "verify failure":
				engine.err = failure
			case "commit failure":
				f.db.batchErr = failure
			}
			err := f.chain.InsertBlock(block)
			if engine.calls != 1 {
				t.Fatal("unavailable block was not fully reverified")
			}
			wantBody, wantConfirmed, wantTip := "replacement", f.confirmed, block.Hash()
			if stage == "verify failure" || stage == "commit failure" {
				if !errors.Is(err, failure) || len(f.client.broadcasts) != 0 {
					t.Fatal("failed repair lost its error or was broadcast", err)
				}
				wantBody, wantConfirmed, wantTip = "old body", oldConfirmed, f.genesis.Hash()
			} else if err != nil || len(f.client.broadcasts) != 1 {
				t.Fatal("repair failed or was not delivered", err)
			}
			stored := qkcrawdb.ReadRootBlock(f.db, block.Hash())
			confirmed, err := f.chain.GetLastConfirmedShardBlocks(block.Hash())
			if stored == nil || string(stored.TrackingData()) != wantBody || err != nil || !reflect.DeepEqual(confirmed, wantConfirmed) {
				t.Fatal("repair did not atomically replace block and confirmations", stored, confirmed, err)
			}
			if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != wantTip || f.chain.RootTip().Hash() != wantTip {
				t.Fatal("repair left inconsistent tips", hash, err)
			}
		})
	}
}

func TestRootInsertRejectsIncompleteParentConfirmations(t *testing.T) {
	f, _ := newInsertEnv(t)
	parent := f.block(f.genesis, 1)
	f.store(parent, false)
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, parent.Hash(), map[string]common.Hash{"10001": f.confirmed["10001"]}); err != nil {
		t.Fatal(err)
	}
	block := f.block(parent, 2)
	// The candidate omits the missing shard, so body validation alone cannot
	// establish that its inherited cumulative snapshot is complete.
	if err := f.chain.InsertBlock(block); err == nil {
		t.Fatal("incomplete parent confirmations accepted")
	}
	if f.chain.RootTip().Hash() != f.genesis.Hash() || qkcrawdb.HasRootBlock(f.db, block.Hash()) || len(f.client.broadcasts) != 0 {
		t.Fatal("invalid parent snapshot changed state or was broadcast")
	}
}

func TestRootInsertDeliveryResults(t *testing.T) {
	for _, stage := range []string{"broadcast", "clear marker", "cancel rpc", "cancel after success"} {
		t.Run(stage, func(t *testing.T) {
			f, engine := newInsertEnv(t)
			block := f.block(f.genesis, 1)
			failure := errors.New("delivery failed")
			switch stage {
			case "broadcast":
				f.client.broadcastErr = failure
			case "clear marker":
				f.db.deleteErr = failure
			case "cancel rpc":
				failure = context.Canceled
				f.client.onBroadcast = func(ctx context.Context, _ *types.RootBlock) error { f.cancel(); return ctx.Err() }
			case "cancel after success":
				failure = nil
				f.client.onBroadcast = func(context.Context, *types.RootBlock) error { f.cancel(); return nil }
			}
			if err := f.chain.InsertBlock(block); !errors.Is(err, failure) {
				t.Fatal("wrong delivery result", err)
			}
			if f.chain.RootTip().Hash() != block.Hash() || qkcrawdb.ReadRootBlock(f.db, block.Hash()) == nil {
				t.Fatal("delivery failure lost the local commit")
			}
			f.client.broadcastErr, f.client.onBroadcast = nil, nil
			f.db.deleteErr = nil
			if stage == "cancel after success" {
				if !f.chain.initialized.Load() || f.chain.stopping.Load() || f.chain.Ready() {
					t.Fatal("successful delivery must remain recorded without readiness after backend cancellation")
				}
				return
			}
			if failure != nil {
				if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != block.Hash() {
					t.Fatal("failed delivery or cleanup lost its recovery marker", hash, err)
				}
				if !f.chain.initialized.Load() || !f.chain.stopping.Load() || f.chain.Ready() {
					t.Fatal("delivery failure did not preserve initialization and stop the chain")
				}
				before := len(f.client.calls)
				if err := f.chain.Init(); !errors.Is(err, errChainStopped) {
					t.Fatal("Init revived a failed chain", err)
				}
				for _, input := range []*types.RootBlock{block, f.block(block, 2)} {
					if err := f.chain.InsertBlock(input); !errors.Is(err, errChainStopped) {
						t.Fatal("failed chain accepted an import", err)
					}
				}
				if f.chain.Ready() || engine.calls != 1 || len(f.client.calls) != before {
					t.Fatal("failed chain resumed work")
				}
				f.chain.Close()
				// A backend restart creates a new lifetime context.
				f.cancel()
				f.backendCtx, f.cancel = context.WithCancel(context.Background())
				t.Cleanup(f.cancel)
				f.chain = f.open()
				if err := f.chain.Init(); err != nil {
					t.Fatal("restart failed", err)
				}
				if f.client.checkpoints[len(f.client.checkpoints)-1] != block.Hash() {
					t.Fatal("restart did not initialize shards at the committed tip")
				}
			} else if !f.chain.Ready() {
				t.Fatal("successful delivery disabled the chain")
			}
			if err := f.chain.InsertBlock(block); err != nil || engine.calls != 1 {
				t.Fatal("known-block retry failed or reverified", err)
			}
		})
	}
}

func TestRootInsertRequiresReadyAndOpenChain(t *testing.T) {
	f := newRootEnv(t)
	block := f.block(f.genesis, 1)
	if err := f.chain.InsertBlock(block); !errors.Is(err, errChainNotInitialized) {
		t.Fatal("uninitialized chain accepted an import", err)
	}
	if err := f.chain.Init(); err != nil {
		t.Fatal(err)
	}
	f.cancel()
	if err := f.chain.InsertBlock(block); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled import accepted", err)
	}
	for _, invalid := range []*types.RootBlock{nil, f.genesis} {
		if err := f.chain.InsertBlock(invalid); err == nil {
			t.Fatal("nil or genesis import accepted")
		}
	}
	f.chain.Close()
	if err := f.chain.InsertBlock(block); !errors.Is(err, errChainStopped) {
		t.Fatal("closed chain accepted an import", err)
	}
	if len(f.client.broadcasts) != 0 {
		t.Fatal("rejected import was broadcast")
	}
}

func TestRootInsertSerializesWithInit(t *testing.T) {
	for _, first := range []string{"init", "delivery", "delivery failure"} {
		t.Run(first, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, engine := newInsertEnv(t)
				a := f.block(f.genesis, 1)
				b := f.block(a, 2)
				started, release := make(chan struct{}), make(chan struct{})
				results := make(chan error, 3)
				var failure error
				if first == "delivery failure" {
					failure = errors.New("broadcast failed")
				}
				wantCalls := 1
				if first == "init" {
					f.chain = f.open()
					b, wantCalls = a, 0
					f.client.onPing = func(context.Context, *types.RootBlock) error {
						close(started)
						<-release
						return nil
					}
					go func() { results <- f.chain.Init() }()
				} else {
					f.client.onBroadcast = func(_ context.Context, block *types.RootBlock) error {
						if block.Hash() == a.Hash() {
							close(started)
							<-release
						}
						return failure
					}
					go func() { results <- f.chain.InsertBlock(a) }()
				}
				<-started
				go func() { results <- f.chain.InsertBlock(b) }()
				go func() { results <- f.chain.Init() }()
				synctest.Wait()
				if engine.calls != wantCalls || qkcrawdb.HasRootBlock(f.db, b.Hash()) {
					t.Fatal("second insertion ran before first delivery completed")
				}
				close(release)
				if failure != nil {
					failures, stopped := 0, 0
					for range 3 {
						switch err := <-results; {
						case errors.Is(err, failure):
							failures++
						case errors.Is(err, errChainStopped):
							stopped++
						default:
							t.Fatal("unexpected concurrent result", err)
						}
					}
					if failures != 1 || stopped != 2 || f.chain.Ready() {
						t.Fatal("failed delivery did not disable queued operations")
					}
					if engine.calls != 1 || len(f.client.calls) != 2 || qkcrawdb.HasRootBlock(f.db, b.Hash()) || f.chain.RootTip().Hash() != a.Hash() {
						t.Fatal("queued operations changed the committed chain after delivery failed")
					}
					return
				}
				for range 3 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				if engine.calls != wantCalls+1 || f.chain.RootTip().Hash() != b.Hash() {
					t.Fatal("serialized insertion did not finish")
				}
			})
		})
	}
}
