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
	"time"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/consensus/poswshard"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
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
	onHeaders    func(context.Context) error
	broadcastErr error
	onBroadcast  func(context.Context, *types.RootBlock) error
	onStakes     func(context.Context, *big.Int, common.Address, common.Hash) (*clusternet.RootChainStakesResult, error)
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

func (c *rootClient) GetUnconfirmedHeaderList(ctx context.Context) (map[string][]*coretypes.Header, error) {
	c.calls = append(c.calls, "headers")
	if c.onHeaders != nil {
		return nil, c.onHeaders(ctx)
	}
	return nil, ctx.Err()
}

func (c *rootClient) GetRootChainStakes(ctx context.Context, chainID *big.Int, coinbase common.Address, hash common.Hash) (*clusternet.RootChainStakesResult, error) {
	c.calls = append(c.calls, "stakes")
	if c.onStakes != nil {
		return c.onStakes(ctx, chainID, coinbase, hash)
	}
	return nil, nil
}

// Inject storage failures during recovery, initialization and insertion.
type rootDB struct {
	ethdb.Database
	readErr        error
	hasErr         error
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

func (db *rootDB) Has(key []byte) (bool, error) {
	if db.hasErr != nil {
		return false, db.hasErr
	}
	return db.Database.Has(key)
}

func (db *rootDB) Put(key, value []byte) error {
	if db.putErr != nil {
		return db.putErr
	}
	return db.Database.Put(key, value)
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
	// An old parent brings a candidate down to difficulty one for the Ethash test.
	f.config.PoSW.AdjustmentFactor = 1
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

func TestRootCreateBlockToMine(t *testing.T) {
	f := newRootEnv(t)
	parent := f.block(f.genesis, 1)
	f.store(parent, true)
	chain := f.open()
	coinbase := common.Address{42}
	if block, err := chain.CreateBlockToMine(t.Context(), coinbase); block != nil || !errors.Is(err, errChainNotReady) {
		t.Fatalf("before Init: block %v, error %v", block, err)
	}
	if err := chain.Init(); err != nil {
		t.Fatal(err)
	}
	block, err := chain.CreateBlockToMine(t.Context(), coinbase)
	if err != nil {
		t.Fatal(err)
	}
	if block.ParentHash() != parent.Hash() || block.Number() != parent.Number()+1 || block.Coinbase() != coinbase {
		t.Fatal("candidate does not extend the root tip or pay the requested coinbase")
	}
	if len(f.client.calls) != 2 || f.client.calls[1] != "headers" {
		t.Fatalf("expected a shard header request after initialization, got %v", f.client.calls)
	}
	difficulty := block.Difficulty().ToBig()
	if difficulty.Cmp(common.Big1) != 0 {
		t.Fatalf("candidate difficulty %v, want 1", difficulty)
	}
	miner := &poswshard.PoSW{EthashVerifier: poswshard.NewEthashVerifier()}
	defer miner.Close()
	nonce, mix, ok := miner.SearchNonce(t.Context(), block.NumberU64(), f.engine.SealHash(block.Header()), difficulty, 0)
	if !ok {
		t.Fatal("Ethash search failed")
	}
	sealed := block.WithMiningResult(nonce.Uint64(), mix, nil)
	if err := f.engine.VerifyBlock(chain, sealed); err != nil {
		t.Fatalf("valid Ethash seal rejected: %v", err)
	}
	mix[0] ^= 1
	if err := f.engine.VerifyBlock(chain, block.WithMiningResult(nonce.Uint64(), mix, nil)); err == nil {
		t.Fatal("invalid Ethash mix digest accepted")
	}
	if block.Nonce() != 0 || block.MixDigest() != (common.Hash{}) || block.Signature() != ([65]byte{}) {
		t.Fatal("original candidate is sealed or signed")
	}
	if chain.RootTip().Hash() != parent.Hash() || qkcrawdb.HasRootBlock(f.db, block.Hash()) || len(f.client.broadcasts) != 0 {
		t.Fatal("candidate construction advanced the tip, stored or broadcast the block")
	}
}

func TestRootCreateBlockToMineWaitsForDelivery(t *testing.T) {
	for _, test := range []struct {
		name        string
		deliveryErr error
	}{
		{name: "success"},
		{name: "failure", deliveryErr: errors.New("root delivery failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, _ := newInsertEnv(t)
				parent := f.block(f.genesis, 1)
				delivering, release := make(chan struct{}), make(chan struct{})
				f.client.onBroadcast = func(context.Context, *types.RootBlock) error {
					close(delivering)
					<-release
					return test.deliveryErr
				}
				inserted := make(chan error, 1)
				go func() { inserted <- f.chain.InsertBlock(parent) }()
				<-delivering

				type result struct {
					block *types.RootBlock
					err   error
				}
				built := make(chan result, 1)
				go func() {
					block, err := f.chain.CreateBlockToMine(t.Context(), common.Address{})
					built <- result{block, err}
				}()
				synctest.Wait()
				if len(built) != 0 || len(f.client.calls) != 2 {
					t.Errorf("mining proceeded before root delivery completed: calls %v", f.client.calls)
				}
				close(release)
				if err := <-inserted; !errors.Is(err, test.deliveryErr) {
					t.Fatal("unexpected insert result", err)
				}
				got := <-built
				if test.deliveryErr != nil {
					if got.block != nil || !errors.Is(got.err, errChainNotReady) || len(f.client.calls) != 2 {
						t.Fatalf("mining after failed delivery: block %v, error %v, calls %v", got.block, got.err, f.client.calls)
					}
					return
				}
				if got.err != nil || got.block == nil || got.block.ParentHash() != parent.Hash() {
					t.Fatalf("mining after delivery: block %v, error %v", got.block, got.err)
				}
			})
		})
	}
}

func TestRootCreateBlockToMineCancelsShardRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		if err := f.chain.Init(); err != nil {
			t.Fatal(err)
		}
		f.client.onHeaders = func(ctx context.Context) error {
			f.cancel()
			<-ctx.Done()
			return ctx.Err()
		}
		if block, err := f.chain.CreateBlockToMine(t.Context(), common.Address{}); block != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("backend shutdown during shard request: block %v, error %v", block, err)
		}
		if block, err := f.chain.CreateBlockToMine(t.Context(), common.Address{}); block != nil || !errors.Is(err, errChainNotReady) {
			t.Fatalf("new request after backend shutdown: block %v, error %v", block, err)
		}
	})
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
					write = func() error { return f.chain.InsertBlock(block) }
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
				if err := f.chain.InsertBlock(block); !errors.Is(err, errChainStopped) {
					t.Fatal("closed chain accepted insertion", err)
				}
				if candidate, err := f.chain.CreateBlockToMine(t.Context(), common.Address{}); candidate != nil || !errors.Is(err, errChainNotReady) {
					t.Fatalf("mining after Close: candidate %v, error %v", candidate, err)
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

func TestRootNewRequiresBackendContext(t *testing.T) {
	f := newRootEnv(t)
	if chain, err := New(nil, f.config, f.client, f.db, f.engine); chain != nil || err == nil {
		t.Fatal("missing backend context accepted", err)
	}
}

func TestRootShardIncomePersistenceAndErrors(t *testing.T) {
	f := newRootEnv(t)
	hash := common.Hash{9}
	if err := f.chain.AddShardBlockCoinbaseAmount(hash, uint256.NewInt(0)); err != nil {
		t.Fatal(err)
	}
	// Income reports must not reenter the chain write lock, and must survive a
	// RootChain restart independently of whether their shard block is canonical.
	f.chain = f.open()
	if !f.chain.chainmu.TryLock() {
		t.Fatal("chain already closed")
	}
	defer f.chain.chainmu.Unlock()
	if amount, err := f.chain.GetShardBlockCoinbaseAmount(hash); err != nil || amount == nil || !amount.IsZero() {
		t.Fatal("stored zero income was lost", amount, err)
	}
	if err := f.chain.AddShardBlockCoinbaseAmount(hash, uint256.NewInt(42)); err != nil {
		t.Fatal(err)
	}
	if amount, err := f.chain.GetShardBlockCoinbaseAmount(hash); err != nil || amount == nil || amount.Uint64() != 42 {
		t.Fatal("repeated income report did not overwrite", amount, err)
	}
	failure := errors.New("income storage failed")
	f.db.putErr = failure
	if err := f.chain.AddShardBlockCoinbaseAmount(hash, uint256.NewInt(99)); !errors.Is(err, failure) {
		t.Fatal("write error lost", err)
	}
	f.db.putErr = nil
	for _, stage := range []string{"has", "get"} {
		if stage == "has" {
			f.db.hasErr = failure
		} else {
			f.db.readErr = failure
		}
		if amount, err := f.chain.GetShardBlockCoinbaseAmount(hash); amount != nil || !errors.Is(err, failure) {
			t.Fatal("failed report read was treated as unreported", stage, amount, err)
		}
		f.db.hasErr, f.db.readErr = nil, nil
	}
	if amount, err := f.chain.GetShardBlockCoinbaseAmount(hash); err != nil || amount == nil || amount.Uint64() != 42 {
		t.Fatal("failed write changed persisted income", amount, err)
	}
	if amount, err := f.chain.GetShardBlockCoinbaseAmount(common.Hash{10}); amount != nil || err != nil {
		t.Fatal("unreported block returned data or error", amount, err)
	}
}

func TestRootTipWithLockWaitsForWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRootEnv(t)
		if tip, err := f.chain.GetRootTipWithLock(); err != nil || tip.Hash() != f.genesis.Hash() || f.chain.Ready() {
			t.Fatal("locked tip should also be available before Init", tip, err)
		}
		if !f.chain.chainmu.TryLock() {
			t.Fatal("chain already closed")
		}
		// Model an import which has published its local tip but still holds the
		// write lock while delivering the root to shards.
		next := f.block(f.genesis, 1).Header()
		f.chain.tip.Store(next)
		tips := make(chan *types.RootBlockHeader, 1)
		errs := make(chan error, 1)
		go func() {
			tip, err := f.chain.GetRootTipWithLock()
			tips <- tip
			errs <- err
		}()
		synctest.Wait()
		select {
		case <-tips:
			t.Fatal("locked tip returned before delivery finished")
		default:
		}
		if f.chain.RootTip() != next {
			t.Fatal("atomic tip query blocked or lost the locally published tip")
		}
		f.chain.chainmu.Unlock()
		if tip, err := <-tips, <-errs; tip != next || err != nil {
			t.Fatal("wrong tip after delivery", tip, err)
		}
		f.chain.Close()
		if tip, err := f.chain.GetRootTipWithLock(); tip != nil || err != errChainStopped {
			t.Fatal("closed chain returned a locked tip", tip, err)
		}
	})
}

func TestRootChainStakeQuery(t *testing.T) {
	f := newRootEnv(t)
	id, hash, coinbase := big.NewInt(10002), common.Hash{9}, common.Address{10}
	want := &clusternet.RootChainStakesResult{Stakes: uint256.NewInt(123), Signer: common.Address{11}}
	var rpcCtx context.Context
	f.client.onStakes = func(ctx context.Context, gotID *big.Int, gotCoinbase common.Address, gotHash common.Hash) (*clusternet.RootChainStakesResult, error) {
		rpcCtx = ctx
		if gotID.Cmp(id) != 0 || gotCoinbase != coinbase || gotHash != hash {
			t.Fatal("stake query changed the requested shard, block or account")
		}
		if deadline, ok := ctx.Deadline(); !ok || !deadline.After(time.Now()) {
			t.Fatal("stake query has no usable deadline")
		}
		// A shard can report a block while consensus is waiting for its RPC.
		if err := f.chain.AddShardBlockCoinbaseAmount(hash, uint256.NewInt(0)); err != nil {
			t.Fatal(err)
		}
		return want, nil
	}
	if !f.chain.chainmu.TryLock() {
		t.Fatal("chain already closed")
	}
	amount, signer, err := f.chain.GetRootChainStakes(id, hash, coinbase)
	f.chain.chainmu.Unlock()
	if err != nil || amount == nil || !amount.Eq(want.Stakes) || signer != want.Signer {
		t.Fatal("stake response changed", amount, signer, err)
	}
	if !errors.Is(rpcCtx.Err(), context.Canceled) {
		t.Fatal("stake query context leaked after returning")
	}
	failure := &wire.RemoteError{Code: 7}
	for _, stage := range []string{"zero", "missing response", "missing stakes", "rpc error"} {
		f.client.onStakes = func(context.Context, *big.Int, common.Address, common.Hash) (*clusternet.RootChainStakesResult, error) {
			switch stage {
			case "missing response":
				return nil, nil
			case "missing stakes":
				return &clusternet.RootChainStakesResult{}, nil
			case "rpc error":
				return nil, failure
			default:
				return &clusternet.RootChainStakesResult{Stakes: uint256.NewInt(0)}, nil
			}
		}
		amount, signer, err := f.chain.GetRootChainStakes(id, hash, coinbase)
		if stage == "zero" {
			if err != nil || amount == nil || !amount.IsZero() || signer != (common.Address{}) {
				t.Fatal("valid zero stakes or unset signer rejected", amount, signer, err)
			}
		} else if amount != nil || err == nil || (stage == "rpc error" && !errors.Is(err, failure)) {
			t.Fatal("missing or failed stake response accepted", stage, amount, err)
		}
		if stage == "rpc error" {
			var remote *wire.RemoteError
			if !errors.As(err, &remote) || remote.Code != failure.Code {
				t.Fatal("remote error code lost after wrapping", err)
			}
		}
	}
}

func TestRootChainStakeQueryCancellation(t *testing.T) {
	for _, stage := range []string{"already canceled", "backend shutdown", "timeout"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newRootEnv(t)
				if stage == "already canceled" {
					f.cancel()
				}
				f.client.onStakes = func(ctx context.Context, _ *big.Int, _ common.Address, _ common.Hash) (*clusternet.RootChainStakesResult, error) {
					if stage == "backend shutdown" {
						f.cancel()
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
				want := context.Canceled
				if stage == "timeout" {
					want = context.DeadlineExceeded
				}
				if amount, _, err := f.chain.GetRootChainStakes(big.NewInt(10001), common.Hash{9}, common.Address{}); amount != nil || !errors.Is(err, want) {
					t.Fatal("stake query did not observe cancellation", amount, err)
				}
				if stage == "already canceled" && len(f.client.calls) != 0 {
					t.Fatal("canceled backend started a cluster RPC")
				}
				if stage == "timeout" {
					// A read timeout expires only this query, not the backend or
					// future queries against the same confirmed shard state.
					f.client.onStakes = func(ctx context.Context, _ *big.Int, _ common.Address, _ common.Hash) (*clusternet.RootChainStakesResult, error) {
						return &clusternet.RootChainStakesResult{Stakes: uint256.NewInt(1)}, ctx.Err()
					}
					if amount, _, err := f.chain.GetRootChainStakes(big.NewInt(10001), common.Hash{9}, common.Address{}); err != nil || amount == nil || amount.Uint64() != 1 {
						t.Fatal("query timeout affected subsequent stake queries", amount, err)
					}
				}
			})
		})
	}
}
