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
	"fmt"
	"math/big"
	"testing"
	"testing/synctest"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// Record cluster startup RPCs and inject failures without real connections.
type rootClient struct {
	ClusterClient
	calls        []string
	checkpoints  []common.Hash
	broadcasts   []common.Hash
	pingErr      error
	broadcastErr error
	onPing       func(context.Context, *types.RootBlock) error
	onBroadcast  func(context.Context, *types.RootBlock) error
}

func (c *rootClient) Ping(ctx context.Context, block *types.RootBlock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.calls = append(c.calls, "ping")
	c.checkpoints = append(c.checkpoints, block.Hash())
	if c.onPing != nil {
		return c.onPing(ctx, block)
	}
	return c.pingErr
}

func (c *rootClient) BroadcastRootBlock(ctx context.Context, block *types.RootBlock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.calls = append(c.calls, "broadcast")
	c.broadcasts = append(c.broadcasts, block.Hash())
	if c.onBroadcast != nil {
		return c.onBroadcast(ctx, block)
	}
	return c.broadcastErr
}

// Inject storage failures during recovery and initialization.
type rootDB struct {
	ethdb.Database
	readErr   error
	deleteErr error
}

func (db *rootDB) Delete(key []byte) error {
	if db.deleteErr != nil {
		return db.deleteErr
	}
	return db.Database.Delete(key)
}

func (db *rootDB) Get(key []byte) ([]byte, error) {
	if db.readErr != nil {
		return nil, db.readErr
	}
	return db.Database.Get(key)
}

type rootEnv struct {
	t         *testing.T
	config    *params.QuarkChainConfig
	db        *rootDB
	client    *rootClient
	engine    posw.RootEngine
	chain     *RootChain
	genesis   *types.RootBlock
	confirmed map[string]common.Hash
}

// Seed the DB directly: genesis creation and root insertion are outside this PR.
// Unlike Python's empty-DB startup, root genesis already confirms every member.
func newRootEnv(t *testing.T) *rootEnv {
	t.Helper()
	f := &rootEnv{
		t: t, config: params.NewTestQuarkChainConfig(10001, 10001, 10002).QuarkChain,
		db: &rootDB{Database: rawdb.NewMemoryDatabase()}, client: new(rootClient),
		confirmed: make(map[string]common.Hash),
	}
	t.Cleanup(func() { f.db.Close() })
	engine, err := posw.New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	t.Cleanup(func() { engine.Close() })
	var headers types.ShardBlockHeaders
	for _, id := range f.config.ChainIDs {
		header := &coretypes.Header{
			Number: new(big.Int), Difficulty: new(big.Int), Time: 1,
			Extra: (&coretypes.QKCExtra{ChainID: id}).Encode(),
		}
		headers = append(headers, header)
		f.confirmed[id.String()] = header.Hash()
	}
	f.genesis = types.NewRootBlock(&types.RootBlockHeader{
		Time: 1, Difficulty: *uint256.NewInt(100), TotalDifficulty: *uint256.NewInt(100),
	}, headers, nil)
	f.store(f.genesis, true)
	f.chain = f.open()
	return f
}

func (f *rootEnv) open() *RootChain {
	f.t.Helper()
	c, err := New(f.config, f.client, f.db, f.engine)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// These are stored recovery fixtures; building or mining roots is not exercised.
func (f *rootEnv) block(parent *types.RootBlock, seed byte) *types.RootBlock {
	f.t.Helper()
	header := &types.RootBlockHeader{
		Number: parent.Number() + 1, ParentHash: parent.Hash(),
		Time: parent.Time() + 1, Extra: []byte{seed}, Difficulty: *uint256.NewInt(100),
	}
	header.TotalDifficulty.Add(parent.TotalDifficulty(), &header.Difficulty)
	return types.NewRootBlock(header, nil, nil)
}

// Seed side branches without changing the head that New will load.
func (f *rootEnv) store(block *types.RootBlock, canonical bool) {
	f.t.Helper()
	batch := f.db.NewBatch()
	defer batch.Close()
	qkcrawdb.WriteRootBlock(batch, block)
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(batch, block.Hash(), f.confirmed); err != nil {
		f.t.Fatal(err)
	}
	if canonical {
		if err := qkcrawdb.WriteRootCanonicalHash(batch, block.Hash(), block.Number()); err != nil {
			f.t.Fatal(err)
		}
		if err := qkcrawdb.WriteRootHeadBlockHash(batch, block.Hash()); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := batch.Write(); err != nil {
		f.t.Fatal(err)
	}
}

// Genesis is immediately available without cluster initialization.
// The fixture supplies genesis instead of running RootState.__create_genesis_block.
func TestRootGenesisRecovery(t *testing.T) {
	f := newRootEnv(t)
	if f.chain.engine != f.engine || f.chain.client != f.client || f.chain.db != f.db {
		t.Fatal("New did not use the supplied engine, client and database")
	}
	tip := f.chain.RootTip()
	if tip == nil || tip.Hash() != f.genesis.Hash() {
		t.Fatal("New did not recover genesis")
	}
	if block, err := f.chain.GetRootBlockByNumber(0); err != nil || block == nil || block.Hash() != f.genesis.Hash() {
		t.Fatal("genesis block is not available", err)
	}
	if header, err := f.chain.GetRootHeaderByNumber(0); err != nil || header == nil || header.Hash() != f.genesis.Hash() {
		t.Fatal("genesis header is not available", err)
	}
	if len(f.client.calls) != 0 {
		t.Fatal("local recovery made cluster RPCs")
	}
}

// RootDb.__recover_from_db selects A from persisted head B without rewriting
// tipHash. Repeated restarts must select A again rather than stepping back further.
// Number reads follow the stored canonical index, independently of the checkpoint.
// Hash reads also retain side branches.
func TestRootRecoveryKeepsCheckpoint(t *testing.T) {
	f := newRootEnv(t)
	a := f.block(f.genesis, 1)
	f.store(a, true)
	b := f.block(a, 2)
	f.store(b, true)
	side := f.block(f.genesis, 3)
	f.store(side, false)
	for range 2 {
		c := f.open()
		if c.RootTip().Hash() != a.Hash() {
			t.Fatal("restart selected the wrong checkpoint")
		}
		if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != b.Hash() {
			t.Fatal("recovery changed the persisted head")
		}
		if block, err := c.GetRootBlockByNumber(a.Number()); err != nil || block == nil || block.Hash() != a.Hash() {
			t.Fatal("checkpoint is not available by number", err)
		}
		if block, err := c.GetRootBlockByNumber(b.Number()); err != nil || block == nil || block.Hash() != b.Hash() {
			t.Fatal("number query lost the stored canonical head", err)
		}
		if header, err := c.GetRootHeaderByNumber(b.Number()); err != nil || header == nil || header.Hash() != b.Hash() {
			t.Fatal("number query lost the stored canonical header", err)
		}
		for _, want := range []*types.RootBlock{b, side} {
			if block, err := c.GetRootBlock(want.Hash()); err != nil || block == nil || block.Hash() != want.Hash() {
				t.Fatal("hash query lost a stored branch", err)
			}
		}
	}
}

// A persisted head must reference a stored block; New does not recreate it.
// goquarkchain loadLastState can Reset here; this PR requires external DB setup.
func TestRootNewRejectsMissingHead(t *testing.T) {
	f := newRootEnv(t)
	if err := qkcrawdb.WriteRootHeadBlockHash(f.db, common.Hash{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.config, f.client, f.db, f.engine); !errors.Is(err, ErrRootDataMissing) {
		t.Fatal("missing head block accepted", err)
	}
}

// Local recovery failures are constructor errors. Repairing the database permits
// another construction attempt without advancing or rewinding the stored head.
func TestRootNewRejectsMissingRecoveryData(t *testing.T) {
	for _, missing := range []string{"parent block", "parent confirmation", "shard confirmation"} {
		t.Run(missing, func(t *testing.T) {
			f := newRootEnv(t)
			a := f.block(f.genesis, 1)
			switch missing {
			case "parent confirmation":
				f.store(a, false)
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, a.Hash(), nil); err != nil {
					t.Fatal(err)
				}
			case "shard confirmation":
				f.store(a, false)
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, a.Hash(), map[string]common.Hash{"10001": f.confirmed["10001"]}); err != nil {
					t.Fatal(err)
				}
			}
			b := f.block(a, 2)
			f.store(b, true)
			chain, err := New(f.config, f.client, f.db, f.engine)
			if err == nil || chain != nil {
				t.Fatal("missing recovery data accepted", err)
			}
			if missing == "parent block" && !errors.Is(err, ErrRootDataMissing) {
				t.Fatal("missing parent block lost its error classification", err)
			}
			f.store(a, false)
			if c := f.open(); c.RootTip().Hash() != a.Hash() {
				t.Fatal("recovery after repair selected the wrong checkpoint")
			}
			if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != b.Hash() {
				t.Fatal("recovery changed the persisted head", err)
			}
		})
	}
}

func TestRootNewRejectsUnexpectedConfirmationChains(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "extra chain"
		if replace {
			name = "replacement chain"
		}
		t.Run(name, func(t *testing.T) {
			f := newRootEnv(t)
			confirmed := map[string]common.Hash{
				"10001": f.confirmed["10001"],
				"10002": f.confirmed["10002"],
				"10003": {1},
			}
			if replace {
				delete(confirmed, "10002")
			}
			if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, f.genesis.Hash(), confirmed); err != nil {
				t.Fatal(err)
			}
			if chain, err := New(f.config, f.client, f.db, f.engine); err == nil || chain != nil {
				t.Fatal("unexpected confirmation chain accepted", err)
			}
		})
	}
}

func TestRootReadErrors(t *testing.T) {
	f := newRootEnv(t)
	if confirmed, err := f.chain.GetLastConfirmedShardBlocks(common.Hash{1}); confirmed != nil || err == nil {
		t.Fatal("missing confirmations returned data or no error", confirmed, err)
	}
	failure := errors.New("database failed")
	f.db.readErr = failure
	if chain, err := New(f.config, f.client, f.db, f.engine); !errors.Is(err, failure) || chain != nil {
		t.Fatal("head read failure accepted", err)
	}
	if confirmed, err := f.chain.GetLastConfirmedShardBlocks(f.genesis.Hash()); confirmed != nil || !errors.Is(err, failure) || errors.Is(err, ErrRootDataMissing) {
		t.Fatal("confirmation read failure misclassified or lost its cause", confirmed, err)
	}
}

func TestRootInitGenesis(t *testing.T) {
	f := newRootEnv(t)
	if f.chain.Ready() {
		t.Fatal("New initialized the cluster")
	}
	if err := f.chain.Init(context.Background()); err != nil || !f.chain.Ready() {
		t.Fatal("cluster initialization failed", err)
	}
	if len(f.client.checkpoints) != 1 || f.client.checkpoints[0] != f.genesis.Hash() || len(f.client.broadcasts) != 0 {
		t.Fatal("genesis startup did not only initialize shards")
	}
	f.db.readErr = errors.New("successful Init should not read again")
	if err := f.chain.Init(context.Background()); err != nil || len(f.client.calls) != 1 {
		t.Fatal("successful Init was not idempotent", err)
	}
}

// PING uses the checkpoint already selected by New. Pending replay must not
// rewind the tip again, change the persisted head or publish readiness early.
func TestRootInitReplaysPending(t *testing.T) {
	f := newRootEnv(t)
	a := f.block(f.genesis, 1).WithBody(nil, []byte("checkpoint"))
	f.store(a, true)
	pending := f.block(a, 2)
	f.store(pending, true)
	if err := qkcrawdb.WriteRootCommittingHash(f.db, pending.Hash()); err != nil {
		t.Fatal(err)
	}
	c := f.open()
	if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != pending.Hash() || len(f.client.calls) != 0 {
		t.Fatal("local recovery changed pending delivery or made cluster RPCs", err)
	}
	f.client.onPing = func(_ context.Context, block *types.RootBlock) error {
		if c.Ready() || block.Hash() != a.Hash() || string(block.TrackingData()) != "checkpoint" {
			t.Fatal("PING did not carry the recovered checkpoint before readiness")
		}
		if c.RootTip().Hash() != a.Hash() {
			t.Fatal("local tip was unavailable before initialization")
		}
		return nil
	}
	f.client.onBroadcast = func(_ context.Context, block *types.RootBlock) error {
		if len(f.client.calls) != 2 || f.client.calls[0] != "ping" || block.Hash() != pending.Hash() || c.Ready() {
			t.Fatal("pending replay ran out of order or published readiness early")
		}
		if header, err := c.GetRootHeader(block.ParentHash()); err != nil || header == nil {
			t.Fatal("root reader callback failed", err)
		}
		return nil
	}
	if err := c.Init(context.Background()); err != nil || !c.Ready() || c.RootTip().Hash() != a.Hash() {
		t.Fatal("startup replay failed", err)
	}
	if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != (common.Hash{}) {
		t.Fatal("startup left pending delivery", err)
	}
	if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != pending.Hash() {
		t.Fatal("startup changed the persisted head", err)
	}
}

// RPC and marker failures keep the chain unready and retain pending delivery.
func TestRootInitFailureAndRetry(t *testing.T) {
	for _, stage := range []string{"ping", "broadcast", "clear marker", "read marker", "missing block", "cancel before init", "cancel ping", "cancel broadcast"} {
		t.Run(stage, func(t *testing.T) {
			f := newRootEnv(t)
			pending := f.block(f.genesis, 1)
			if stage != "missing block" {
				f.store(pending, false)
			}
			if err := qkcrawdb.WriteRootCommittingHash(f.db, pending.Hash()); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("startup failed")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "ping":
				f.client.pingErr = failure
			case "broadcast":
				f.client.broadcastErr = failure
			case "clear marker":
				f.db.deleteErr = failure
			case "read marker":
				f.client.onPing = func(context.Context, *types.RootBlock) error {
					f.db.readErr = failure
					return nil
				}
			case "missing block":
				failure = ErrRootDataMissing
				f.chain = f.open()
				if len(f.client.calls) != 0 {
					t.Fatal("local recovery made cluster RPCs")
				}
			case "cancel before init":
				failure = context.Canceled
				cancel()
			case "cancel ping":
				failure = context.Canceled
				f.client.onPing = func(ctx context.Context, _ *types.RootBlock) error { cancel(); return ctx.Err() }
			case "cancel broadcast":
				failure = context.Canceled
				f.client.onBroadcast = func(ctx context.Context, _ *types.RootBlock) error { cancel(); return ctx.Err() }
			}
			if err := f.chain.Init(ctx); !errors.Is(err, failure) || f.chain.Ready() {
				t.Fatal("failed startup lost the error or published readiness", err)
			}
			if stage != "broadcast" && stage != "clear marker" && stage != "cancel broadcast" && len(f.client.broadcasts) != 0 {
				t.Fatal("pending replay ran after an earlier failure")
			}
			f.client.pingErr, f.client.broadcastErr, f.client.onPing = nil, nil, nil
			f.client.onBroadcast = nil
			f.db.readErr, f.db.deleteErr = nil, nil
			if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != pending.Hash() {
				t.Fatal("failure lost pending delivery", err)
			}
			if stage == "missing block" {
				f.store(pending, false)
			}
			if err := f.chain.Init(context.Background()); err != nil || !f.chain.Ready() || f.chain.RootTip().Hash() != f.genesis.Hash() {
				t.Fatal("startup retry failed", err)
			}
			if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != (common.Hash{}) {
				t.Fatal("retry left pending delivery", err)
			}
		})
	}
}

// Waiting callers check cancellation or reuse the result after acquiring the lock.
func TestRootInitConcurrentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		started, release := make(chan struct{}), make(chan struct{})
		f.client.onPing = func(context.Context, *types.RootBlock) error {
			close(started)
			<-release
			return nil
		}
		first := make(chan error, 1)
		go func() { first <- f.chain.Init(context.Background()) }()
		<-started
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelled, successful := make(chan error, 1), make(chan error, 1)
		go func() { cancelled <- f.chain.Init(ctx) }()
		go func() { successful <- f.chain.Init(context.Background()) }()
		synctest.Wait()
		cancel()
		if f.chain.Ready() || f.chain.RootTip() == nil {
			t.Fatal("cluster readiness was not separate from local recovery")
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal("initial startup failed", err)
		}
		if err := <-cancelled; !errors.Is(err, context.Canceled) {
			t.Fatal("Init ignored cancellation after acquiring the lock", err)
		}
		if err := <-successful; err != nil || !f.chain.Ready() || len(f.client.calls) != 1 {
			t.Fatal("waiting caller failed or repeated initialization", err)
		}
	})
}

func TestRootCloseWaitsForInit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		started, release := make(chan struct{}), make(chan struct{})
		f.client.onPing = func(context.Context, *types.RootBlock) error {
			close(started)
			<-release
			return nil
		}
		initialized := make(chan error, 1)
		go func() { initialized <- f.chain.Init(context.Background()) }()
		<-started
		closed := make(chan struct{}, 2)
		go func() {
			f.chain.Close()
			closed <- struct{}{}
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned before initialization finished")
		default:
		}
		// Start the second closer after Wait: sync.Once's mutex wait is not
		// considered durably blocked by synctest.
		go func() {
			f.chain.Close()
			closed <- struct{}{}
		}()
		close(release)
		if err := <-initialized; err != nil {
			t.Fatal(err)
		}
		for range 2 {
			<-closed
		}
		f.chain.Close()
		if f.chain.Ready() {
			t.Fatal("closed chain remained ready")
		}
		if err := f.chain.Init(context.Background()); !errors.Is(err, errChainStopped) {
			t.Fatal("closed chain accepted initialization", err)
		}
	})
}

func TestRootInitFinalizesSuccessfulRPCDespiteCancellation(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay=%t", replay), func(t *testing.T) {
			f := newRootEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if replay {
				pending := f.block(f.genesis, 1)
				f.store(pending, false)
				if err := qkcrawdb.WriteRootCommittingHash(f.db, pending.Hash()); err != nil {
					t.Fatal(err)
				}
				f.client.onBroadcast = func(context.Context, *types.RootBlock) error { cancel(); return nil }
			} else {
				f.client.onPing = func(context.Context, *types.RootBlock) error { cancel(); return nil }
			}
			if err := f.chain.Init(ctx); err != nil || !f.chain.Ready() {
				t.Fatal("successful startup did not finish local finalization", err)
			}
			if hash, err := qkcrawdb.ReadRootCommittingHash(f.db); err != nil || hash != (common.Hash{}) {
				t.Fatal("successful startup left pending delivery", err)
			}
		})
	}
}
