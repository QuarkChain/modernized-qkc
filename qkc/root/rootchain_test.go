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

// Record cluster RPCs and inject failures without real connections.
type rootClient struct {
	ClusterClient
	calls        []string
	checkpoints  []common.Hash
	broadcasts   []common.Hash
	pingErr      error
	onPing       func(context.Context, *types.RootBlock) error
	broadcastErr error
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

// Inject storage failures during recovery, initialization and insertion.
type rootDB struct {
	ethdb.Database
	readErr        error
	nextRead       func() ([]byte, error)
	putErr         error
	batchErr       error
	batchDeleteErr error
}

func (db *rootDB) NewBatch() ethdb.Batch {
	return &rootBatch{Batch: db.Database.NewBatch(), db: db}
}

type rootBatch struct {
	ethdb.Batch
	db *rootDB
}

func (b *rootBatch) Put(key, value []byte) error {
	if b.db.putErr != nil {
		return b.db.putErr
	}
	return b.Batch.Put(key, value)
}

func (b *rootBatch) Delete(key []byte) error {
	if b.db.batchDeleteErr != nil {
		return b.db.batchDeleteErr
	}
	return b.Batch.Delete(key)
}

func (b *rootBatch) Write() error {
	if b.db.batchErr != nil {
		return b.db.batchErr
	}
	return b.Batch.Write()
}

func (db *rootDB) Get(key []byte) ([]byte, error) {
	if db.nextRead != nil {
		read := db.nextRead
		db.nextRead = nil
		return read()
	}
	if db.readErr != nil {
		return nil, db.readErr
	}
	return db.Database.Get(key)
}

type rootEnv struct {
	backendCtx context.Context
	cancel     context.CancelFunc
	t          *testing.T
	config     *params.QuarkChainConfig
	db         *rootDB
	client     *rootClient
	engine     posw.RootEngine
	chain      *RootChain
	genesis    *types.RootBlock
	confirmed  map[string]common.Hash
}

// Seed genesis directly so startup tests do not depend on block insertion.
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
	f.backendCtx, f.cancel = context.WithCancel(context.Background())
	t.Cleanup(f.cancel)
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
	c, err := New(f.backendCtx, f.config, f.client, f.db, f.engine)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// Build root fixtures; consensus validation is exercised separately in poswroot.
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

// Recovery selects the persisted head on every restart, retaining canonical and
// side-branch queries without rewinding the stored head.
func TestRootRecoveryKeepsPersistedTip(t *testing.T) {
	f := newRootEnv(t)
	a := f.block(f.genesis, 1)
	f.store(a, true)
	b := f.block(a, 2)
	f.store(b, true)
	side := f.block(f.genesis, 3)
	f.store(side, false)
	for range 2 {
		c := f.open()
		if c.RootTip().Hash() != b.Hash() {
			t.Fatal("restart did not select the persisted tip")
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
// goquarkchain loadLastState can Reset here; RootChain requires external DB setup.
func TestRootNewRejectsMissingHead(t *testing.T) {
	f := newRootEnv(t)
	if err := qkcrawdb.WriteRootHeadBlockHash(f.db, common.Hash{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.backendCtx, f.config, f.client, f.db, f.engine); !errors.Is(err, ErrRootDataMissing) {
		t.Fatal("missing head block accepted", err)
	}
}

// Local recovery failures are constructor errors. Repairing the database permits
// another construction attempt without advancing or rewinding the stored head.
func TestRootNewRejectsMissingRecoveryData(t *testing.T) {
	for _, missing := range []string{"canonical index", "stale canonical index", "snapshot", "empty snapshot", "shard confirmation", "body commitment", "stale confirmation"} {
		t.Run(missing, func(t *testing.T) {
			f := newRootEnv(t)
			// Two headers from one shard exercise its last-header confirmation;
			// the other shard keeps its inherited genesis confirmation.
			first := coretypes.CopyHeader(f.genesis.ShardBlockHeaders()[0])
			first.Number, first.ParentHash = big.NewInt(1), f.confirmed["10001"]
			last := coretypes.CopyHeader(first)
			last.Number, last.ParentHash = big.NewInt(2), first.Hash()
			headers := types.ShardBlockHeaders{first, last}
			tip := types.NewRootBlock(f.block(f.genesis, 1).Header(), headers, nil)
			f.confirmed["10001"] = last.Hash()
			qkcrawdb.WriteRootBlock(f.db, tip)
			if err := qkcrawdb.WriteRootHeadBlockHash(f.db, tip.Hash()); err != nil {
				t.Fatal(err)
			}
			if missing != "canonical index" {
				hash := tip.Hash()
				if missing == "stale canonical index" {
					hash = f.genesis.Hash()
				}
				if err := qkcrawdb.WriteRootCanonicalHash(f.db, hash, tip.Number()); err != nil {
					t.Fatal(err)
				}
			}
			switch missing {
			case "canonical index", "stale canonical index", "body commitment":
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, tip.Hash(), f.confirmed); err != nil {
					t.Fatal(err)
				}
				if missing == "body commitment" {
					qkcrawdb.WriteRootBlock(f.db, tip.WithBody(headers[:1], nil))
				}
			case "stale confirmation":
				confirmed := map[string]common.Hash{"10001": first.Hash(), "10002": f.confirmed["10002"]}
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, tip.Hash(), confirmed); err != nil {
					t.Fatal(err)
				}
			case "empty snapshot":
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, tip.Hash(), nil); err != nil {
					t.Fatal(err)
				}
			case "shard confirmation":
				if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, tip.Hash(), map[string]common.Hash{"10001": f.confirmed["10001"]}); err != nil {
					t.Fatal(err)
				}
			}
			if chain, err := New(f.backendCtx, f.config, f.client, f.db, f.engine); err == nil || chain != nil {
				t.Fatal("invalid persisted tip recovery data accepted", err)
			}
			qkcrawdb.WriteRootBlock(f.db, tip)
			if err := qkcrawdb.WriteRootCanonicalHash(f.db, tip.Hash(), tip.Number()); err != nil {
				t.Fatal(err)
			}
			if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, tip.Hash(), f.confirmed); err != nil {
				t.Fatal(err)
			}
			if c := f.open(); c.RootTip().Hash() != tip.Hash() {
				t.Fatal("recovery after repair did not select the persisted tip")
			}
			if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != tip.Hash() {
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
			if chain, err := New(f.backendCtx, f.config, f.client, f.db, f.engine); err == nil || chain != nil {
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
	if chain, err := New(f.backendCtx, f.config, f.client, f.db, f.engine); !errors.Is(err, failure) || chain != nil {
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
	if err := f.chain.Init(); err != nil || !f.chain.Ready() {
		t.Fatal("cluster initialization failed", err)
	}
	if len(f.client.checkpoints) != 1 || f.client.checkpoints[0] != f.genesis.Hash() || len(f.client.broadcasts) != 0 {
		t.Fatal("genesis startup did not only initialize shards")
	}
	f.db.readErr = errors.New("successful Init should not read again")
	if err := f.chain.Init(); err != nil || len(f.client.calls) != 1 {
		t.Fatal("successful Init was not idempotent", err)
	}
	f.cancel()
	if f.chain.Ready() {
		t.Fatal("backend cancellation left the chain ready")
	}
	if err := f.chain.Init(); !errors.Is(err, context.Canceled) {
		t.Fatal("Init ignored backend cancellation", err)
	}
}

// PING carries the complete persisted tip. Legacy markers are ignored, even
// when they reference a missing block, and startup does not broadcast roots.
func TestRootInitUsesPersistedTip(t *testing.T) {
	f := newRootEnv(t)
	a := f.block(f.genesis, 1)
	f.store(a, true)
	b := f.block(a, 2).WithBody(nil, []byte("persisted tip"))
	f.store(b, true)
	legacyKey, legacyHash := []byte("qkc_root_committing"), common.Hash{0xff}
	if err := f.db.Put(legacyKey, legacyHash.Bytes()); err != nil {
		t.Fatal(err)
	}
	c := f.open()
	if c.RootTip().Hash() != b.Hash() || c.Ready() || len(f.client.calls) != 0 {
		t.Fatal("local recovery did not select the persisted tip before cluster initialization")
	}
	f.client.onPing = func(_ context.Context, block *types.RootBlock) error {
		if c.Ready() || block.Hash() != b.Hash() || string(block.TrackingData()) != "persisted tip" {
			t.Fatal("PING did not carry the complete persisted tip before readiness")
		}
		return nil
	}
	if err := c.Init(); err != nil || !c.Ready() || c.RootTip().Hash() != b.Hash() {
		t.Fatal("initialization at persisted tip failed", err)
	}
	if len(f.client.calls) != 1 || f.client.calls[0] != "ping" || len(f.client.broadcasts) != 0 {
		t.Fatal("startup must only initialize shards with PING at the persisted tip")
	}
	if data, err := f.db.Get(legacyKey); err != nil || common.BytesToHash(data) != legacyHash {
		t.Fatal("startup modified the unused legacy marker", err)
	}
	if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != b.Hash() {
		t.Fatal("startup changed the persisted head", err)
	}
}

// PING failures and cancellation keep the chain unready without changing its tip.
func TestRootInitFailure(t *testing.T) {
	for _, stage := range []string{"ping", "cancel before init", "cancel ping"} {
		t.Run(stage, func(t *testing.T) {
			f := newRootEnv(t)
			failure := errors.New("startup failed")
			switch stage {
			case "ping":
				f.client.pingErr = failure
			case "cancel before init":
				failure = context.Canceled
				f.cancel()
			case "cancel ping":
				failure = context.Canceled
				f.client.onPing = func(ctx context.Context, _ *types.RootBlock) error { f.cancel(); return ctx.Err() }
			}
			if err := f.chain.Init(); !errors.Is(err, failure) || f.chain.Ready() {
				t.Fatal("failed startup lost the error or published readiness", err)
			}
			if f.chain.RootTip().Hash() != f.genesis.Hash() || len(f.client.broadcasts) != 0 {
				t.Fatal("failed startup changed the tip or broadcast a block")
			}
			if stage == "cancel before init" && len(f.client.calls) != 0 {
				t.Fatal("canceled startup made cluster RPCs")
			}
		})
	}
}

// Concurrent callers reuse successful initialization after acquiring the lock.
func TestRootInitConcurrentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		started, release := make(chan struct{}), make(chan struct{})
		f.client.onPing = func(context.Context, *types.RootBlock) error {
			close(started)
			<-release
			return nil
		}
		results := make(chan error, 3)
		go func() { results <- f.chain.Init() }()
		<-started
		for range 2 {
			go func() { results <- f.chain.Init() }()
		}
		synctest.Wait()
		if f.chain.Ready() || f.chain.RootTip() == nil {
			t.Fatal("cluster readiness was not separate from local recovery")
		}
		close(release)
		for range 3 {
			if err := <-results; err != nil {
				t.Fatal("concurrent initialization failed", err)
			}
		}
		if !f.chain.Ready() || len(f.client.calls) != 1 {
			t.Fatal("concurrent callers repeated initialization or lost readiness")
		}
	})
}

func TestRootCloseWaitsForInit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		started, release := make(chan struct{}), make(chan struct{})
		f.client.onPing = func(ctx context.Context, _ *types.RootBlock) error {
			close(started)
			<-ctx.Done()
			<-release // RPC cleanup must finish before Close returns.
			return ctx.Err()
		}
		initialized := make(chan error, 1)
		go func() { initialized <- f.chain.Init() }()
		<-started
		f.cancel() // The backend owns cancellation, before calling Close.
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
		if err := <-initialized; !errors.Is(err, context.Canceled) {
			t.Fatal("backend cancellation did not reach the initialization RPC", err)
		}
		for range 2 {
			<-closed
		}
		f.chain.Close()
		if f.chain.Ready() {
			t.Fatal("closed chain remained ready")
		}
		if err := f.chain.Init(); !errors.Is(err, errChainStopped) {
			t.Fatal("closed chain accepted initialization", err)
		}
	})
}

func TestRootCloseWaitsForWrites(t *testing.T) {
	for _, operation := range []string{"init", "insert"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newRootEnv(t)
				block := f.block(f.genesis, 1)
				started, release := make(chan struct{}), make(chan struct{})
				holdRPC := func(context.Context, *types.RootBlock) error {
					close(started)
					<-release
					return nil
				}
				write := func() error { return f.chain.Init() }
				if operation == "init" {
					f.client.onPing = holdRPC
				} else {
					f.engine = &insertEngine{RootEngine: f.engine}
					f.chain.engine = f.engine
					if err := f.chain.Init(); err != nil {
						t.Fatal(err)
					}
					f.client.onBroadcast = holdRPC
					write = func() error { return f.chain.InsertBlock(context.Background(), block) }
				}
				written := make(chan error, 1)
				go func() { written <- write() }()
				<-started
				if f.chain.Ready() != (operation == "insert") {
					t.Fatal("unexpected readiness before Close")
				}
				closed := make(chan struct{}, 2)
				go func() {
					f.chain.Close()
					closed <- struct{}{}
				}()
				synctest.Wait()
				if !f.chain.stopping.Load() || f.chain.Ready() {
					t.Fatal("Close did not disable readiness before waiting for the write")
				}
				select {
				case <-closed:
					t.Fatal("Close returned before the write finished")
				default:
				}
				// Start the second closer after Wait: sync.Once's mutex wait is not
				// considered durably blocked by synctest.
				go func() {
					f.chain.Close()
					closed <- struct{}{}
				}()
				close(release)
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				for range 2 {
					<-closed
				}
				f.chain.Close()
				if !f.chain.initialized.Load() || f.chain.Ready() {
					t.Fatal("closed chain lost initialization or remained ready")
				}
				if err := f.chain.Init(); !errors.Is(err, errChainStopped) {
					t.Fatal("closed chain accepted initialization", err)
				}
				if err := f.chain.InsertBlock(context.Background(), block); !errors.Is(err, errChainStopped) {
					t.Fatal("closed chain accepted insertion", err)
				}
			})
		})
	}
}

func TestRootInitRecordsSuccessfulPingAfterBackendCancellation(t *testing.T) {
	f := newRootEnv(t)
	f.client.onPing = func(context.Context, *types.RootBlock) error { f.cancel(); return nil }
	if err := f.chain.Init(); err != nil {
		t.Fatal("successful PING did not complete initialization", err)
	}
	if !f.chain.initialized.Load() || f.chain.Ready() {
		t.Fatal("initialization must be recorded without reporting readiness after cancellation")
	}
}
