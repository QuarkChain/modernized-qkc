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

package master

import (
	"context"
	"errors"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet/slavenet"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/root"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// Use the real TCP services; only shard business logic is replaced. Hooks are
// installed before Start and channels coordinate initialization without sleeps.
type testShard struct {
	inits atomic.Int32
	init  func(*qkctypes.RootBlock) error
}

func (s *testShard) Init(block *qkctypes.RootBlock) error {
	s.inits.Add(1)
	if s.init != nil {
		return s.init(block)
	}
	return nil
}
func (*testShard) AddRootBlock(*qkctypes.RootBlock) (bool, error) { return true, nil }
func (*testShard) GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error) {
	return new(uint256.Int), common.Address{}, nil
}
func (*testShard) GetUnconfirmedHeaderList() ([]*coretypes.Header, error)    { return nil, nil }
func (*testShard) AddXshardTxList(common.Hash, []*coretypes.DepositTx) error { return nil }

func newTestNode(t *testing.T, engine string, configure ...func(*node.Config)) *node.Node {
	t.Helper()
	cfg := &node.Config{
		Name: "master-test", DataDir: t.TempDir(), DBEngine: engine,
		P2P: p2p.Config{NoDiscovery: true, NoDial: true}, UseLightweightKDF: true,
	}
	for _, apply := range configure {
		apply(cfg)
	}
	stack, err := node.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

func newTestConfig(t *testing.T, shard *testShard) (*config.RootConfig, *slavenet.Service) {
	t.Helper()
	chain := params.NewTestQuarkChainConfig(10001, 10001).QuarkChain
	chain.ConsensusType = params.ConsensusSimulate
	s, err := slavenet.NewService(slavenet.ServiceConfig{
		ListenAddr: "127.0.0.1:0", LocalChainID: chain.ChainIDs[0],
		ConfiguredChainIDs: chain.ChainIDs, Shutdown: func() {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(shard); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	addr := s.Addr().(*net.TCPAddr)
	shardConfig := params.NewTestQuarkChainConfig(10001, 10001)
	const gasLimit = 1_000_000
	genesis := &core.Genesis{
		Config: shardConfig, Timestamp: 1, GasLimit: gasLimit, Difficulty: new(big.Int),
		Mixhash: common.BigToHash(shardConfig.QuarkChain.PoSW.MinimumDifficulty),
		Alloc:   core.PoSWStakingContractAllocs(shardConfig),
		ExtraData: (&coretypes.QKCExtra{
			ChainID: shardConfig.ChainID, Cursor: coretypes.XShardCursor{RootBlockHeight: 1},
			XShardGasLimit: gasLimit / params.XShardGasLimitDivisor,
		}).Encode(),
	}
	return &config.RootConfig{
		Genesis: &root.RootGenesis{Config: chain, Timestamp: 1, Difficulty: big.NewInt(100)},
		Shards: []config.ShardConfig{{Genesis: genesis,
			Slave: clusternet.SlaveInfo{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: chain.ChainIDs[0]},
		}},
	}, s
}

func testDatabaseOptions() node.DatabaseOptions {
	return node.DatabaseOptions{Cache: 16, Handles: 16}
}

// Seed through the production genesis setup to exercise recovery separately.
func seedRoot(t *testing.T, stack *node.Node, cfg *config.RootConfig) *qkctypes.RootBlock {
	t.Helper()
	db, err := stack.OpenDatabaseWithOptions(stack.Config().DataDir, testDatabaseOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, hash, err := root.SetupRootGenesisBlock(db, cfg.Genesis, cfg.ShardGeneses())
	if err != nil {
		t.Fatal(err)
	}
	return qkcrawdb.ReadRootBlock(db, hash)
}

func newTestBackend(t *testing.T, stack *node.Node, cfg *config.RootConfig) *Backend {
	t.Helper()
	b, err := New(stack, cfg, testDatabaseOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stack.Close(); err != nil && !errors.Is(err, node.ErrNodeStopped) {
			t.Error(err)
		}
	})
	return b
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitBackendReady(t *testing.T, b *Backend) {
	t.Helper()
	ctx := testContext(t)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !b.Ready() {
		select {
		case <-ctx.Done():
			t.Fatal("backend did not become ready")
		case <-b.ctx.Done():
			t.Fatal("backend stopped before becoming ready")
		case <-ticker.C:
		}
	}
}

func waitNodeClosed(t *testing.T, stack *node.Node) {
	t.Helper()
	closed := make(chan struct{})
	go func() { stack.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-testContext(t).Done():
		t.Fatal("node did not shut down")
	}
}

// TestBackendStartupAndReports covers construction without RPC traffic, startup,
// shard report persistence, and resource cleanup through the node lifecycle.
func TestBackendStartupAndReports(t *testing.T) {
	stack := newTestNode(t, "pebble")
	shard := new(testShard)
	cfg, slave := newTestConfig(t, shard)
	genesis, err := root.BuildRootGenesis(cfg.Genesis, cfg.ShardGeneses())
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBackend(t, stack, cfg)
	if b.Ready() || b.RootChain().RootTip().Hash() != genesis.Hash() || b.Engine() == nil {
		t.Fatal("unexpected construction state")
	}
	if shard.inits.Load() != 0 || b.cluster.IsActive() || len(b.Protocols()) != 0 || len(b.APIs()) != 0 {
		t.Fatal("constructor started business or registered unfinished protocols/APIs")
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	if !b.Ready() || shard.inits.Load() != 1 {
		t.Fatal("missing shard initialization")
	}
	if err := stack.Start(); !errors.Is(err, node.ErrNodeRunning) {
		t.Fatalf("second Start: %v", err)
	}
	header := &coretypes.Header{
		Number: big.NewInt(1), Difficulty: new(big.Int), UncleHash: coretypes.EmptyUncleHash,
		Extra: (&coretypes.QKCExtra{ChainID: cfg.Genesis.Config.ChainIDs[0]}).Encode(),
	}
	for _, amount := range []uint64{0, 42, 42} {
		if err := slave.SendShardBlockHeaderToMaster(testContext(t), header, uint256.NewInt(amount)); err != nil {
			t.Fatal(err)
		}
		got, err := b.RootChain().GetShardBlockCoinbaseAmount(header.Hash())
		if err != nil || got == nil || got.Uint64() != amount {
			t.Fatalf("report did not reach RootChain: %v, %v", got, err)
		}
	}
	if err := stack.Close(); err != nil {
		t.Fatal(err)
	}
	if b.Ready() {
		t.Fatal("closed backend remained ready")
	}
	if _, err := qkcrawdb.ReadRootHeadBlockHash(b.db); err == nil {
		t.Fatal("database remained open")
	}
}

// TestBackendNodeCloseBeforeStart checks that either database engine uses DataDir
// directly and node closes it without initialization or invoking Backend.Stop.
func TestBackendNodeCloseBeforeStart(t *testing.T) {
	for _, engine := range []string{"pebble", "leveldb"} {
		t.Run(engine, func(t *testing.T) {
			stack := newTestNode(t, engine)
			shard := new(testShard)
			cfg, _ := newTestConfig(t, shard)
			seedRoot(t, stack, cfg)
			b := newTestBackend(t, stack, cfg)
			if got := rawdb.PreexistingDatabase(stack.Config().DataDir); got != engine {
				t.Fatalf("database not stored directly in DataDir: got %q, want %q", got, engine)
			}
			if err := stack.Close(); err != nil {
				t.Fatal(err)
			}
			if b.Ready() || shard.inits.Load() != 0 {
				t.Fatal("unstarted backend performed initialization")
			}
			if _, err := qkcrawdb.ReadRootHeadBlockHash(b.db); err == nil {
				t.Fatal("node left database open")
			}
		})
	}
}

// TestBackendInitializationFailureClosesNode checks that rejected checkpoints
// and mismatched shard identities shut down the node without publishing readiness.
func TestBackendInitializationFailureClosesNode(t *testing.T) {
	for _, failure := range []string{"checkpoint", "identity"} {
		t.Run(failure, func(t *testing.T) {
			stack := newTestNode(t, "pebble")
			shard := &testShard{init: func(*qkctypes.RootBlock) error { return errors.New("checkpoint rejected") }}
			cfg, _ := newTestConfig(t, shard)
			if failure == "identity" {
				cfg.Genesis.Config.ChainIDs[0] = big.NewInt(999)
				cfg.Shards[0].Slave.ChainID = cfg.Genesis.Config.ChainIDs[0]
				// Change genesis membership while the remote identity stays unchanged.
				cfg.Shards[0].Genesis.Config.ChainID = big.NewInt(999)
				cfg.Shards[0].Genesis.Config.QuarkChain.ChainIDs[0] = big.NewInt(999)
				extra, _ := coretypes.DecodeQKCExtra(cfg.Shards[0].Genesis.ExtraData)
				extra.ChainID = big.NewInt(999)
				cfg.Shards[0].Genesis.ExtraData = extra.Encode()
			}
			b := newTestBackend(t, stack, cfg)
			if err := stack.Start(); err != nil {
				t.Fatal(err)
			}
			waitNodeClosed(t, stack)
			if b.Ready() || shard.inits.Load() > 1 {
				t.Fatal("failed startup became ready or retried Init")
			}
		})
	}
}

// TestBackendStopDuringCheckpointInitialization checks that shutdown interrupts
// an outstanding initialization RPC without waiting for the remote shard handler.
func TestBackendStopDuringCheckpointInitialization(t *testing.T) {
	stack := newTestNode(t, "pebble")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	shard := &testShard{init: func(*qkctypes.RootBlock) error {
		close(entered)
		<-release
		return nil
	}}
	cfg, _ := newTestConfig(t, shard)
	// Registered after slave cleanup so a blocked shard handler is always released first.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-testContext(t).Done():
		t.Fatal("checkpoint initialization did not begin")
	}
	if b.Ready() {
		t.Fatal("ready before shard acknowledgment")
	}
	done := make(chan error, 1)
	go func() { done <- stack.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testContext(t).Done():
		t.Fatal("Stop did not interrupt cluster RPC")
	}
	releaseOnce.Do(func() { close(release) })
	if b.Ready() {
		t.Fatal("canceled initialization published readiness")
	}
}

// TestBackendConstructionFailures checks that failed recovery releases the DB
// and leaves node registration untouched, allowing construction to be retried.
func TestBackendConstructionFailures(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, _ := newTestConfig(t, new(testShard))
	seed := cfg.Genesis
	cfg.Genesis = nil
	if _, err := New(stack, cfg, testDatabaseOptions()); err == nil {
		t.Fatalf("empty database: %v", err)
	}
	// Failure must close the database and leave node registration untouched.
	// Reopening, seeding and constructing on the same stack must work.
	cfg.Genesis = seed
	genesis := seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if b.RootChain().RootTip().Hash() != genesis.Hash() {
		t.Fatal("head not recovered")
	}
	if err := stack.Start(); err != nil {
		t.Fatalf("failed constructor registered a broken lifecycle: %v", err)
	}
	waitBackendReady(t, b)
}

// TestBackendBecomesReadyAfterCheckpointInitialization checks that readiness is
// published only after the shard acknowledges its checkpoint.
func TestBackendBecomesReadyAfterCheckpointInitialization(t *testing.T) {
	stack := newTestNode(t, "pebble")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfg, _ := newTestConfig(t, &testShard{init: func(*qkctypes.RootBlock) error {
		close(entered)
		<-release
		return nil
	}})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-testContext(t).Done():
		t.Fatal("initialization did not begin")
	}
	if b.Ready() {
		t.Fatal("ready before shard acknowledgment")
	}
	once.Do(func() { close(release) })
	waitBackendReady(t, b)
}

// TestBackendEndpointFailureCleanup checks that node releases the backend DB
// when an RPC endpoint fails before backend startup.
func TestBackendEndpointFailureCleanup(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	stack := newTestNode(t, "leveldb", func(cfg *node.Config) {
		cfg.HTTPHost = "127.0.0.1"
		cfg.HTTPPort = occupied.Addr().(*net.TCPAddr).Port
	})
	cfg, _ := newTestConfig(t, new(testShard))
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if err := stack.Start(); err == nil {
		t.Fatal("endpoint startup unexpectedly succeeded")
	}
	if b.Ready() || b.cluster.IsActive() {
		t.Fatal("backend started after endpoint failure")
	}
	if _, err := qkcrawdb.ReadRootHeadBlockHash(b.db); err == nil {
		t.Fatal("node left database open after endpoint failure")
	}
}

// TestBackendDisconnectRequestsShutdown checks that losing an initialized shard
// propagates through the shutdown callback and closes the node.
func TestBackendDisconnectRequestsShutdown(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, slave := newTestConfig(t, new(testShard))
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	if err := slave.Stop(); err != nil {
		t.Fatal(err)
	}
	waitNodeClosed(t, stack)
	if b.Ready() {
		t.Fatal("disconnected backend remained ready")
	}
}

type failedReportDB struct{ ethdb.Database }

func (failedReportDB) Put([]byte, []byte) error { return errors.New("report storage failed") }

// TestBackendReportFailureClosesNode checks that a report storage failure closes
// the connection and node instead of returning a business error code.
func TestBackendReportFailureClosesNode(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, slave := newTestConfig(t, new(testShard))
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	b.chain.Close()
	chain, err := root.New(b.ctx, cfg.Genesis.Config, b.cluster, failedReportDB{b.db}, b.engine)
	if err != nil {
		t.Fatal(err)
	}
	b.chain = chain
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	header := &coretypes.Header{
		Number: big.NewInt(1), Difficulty: new(big.Int), UncleHash: coretypes.EmptyUncleHash,
		Extra: (&coretypes.QKCExtra{ChainID: cfg.Genesis.Config.ChainIDs[0]}).Encode(),
	}
	var remote *wire.RemoteError
	if err := slave.SendShardBlockHeaderToMaster(testContext(t), header, uint256.NewInt(1)); err == nil || errors.As(err, &remote) {
		t.Fatalf("expected connection failure, not a business error code: %v", err)
	}
	waitNodeClosed(t, stack)
	if b.Ready() {
		t.Fatal("backend remained ready after report failure")
	}
}

type blockingReportDB struct {
	ethdb.Database
	entered chan struct{}
	release chan struct{}
	closed  atomic.Bool
	written atomic.Bool
}

func (db *blockingReportDB) Put(key, value []byte) error {
	close(db.entered)
	<-db.release
	err := db.Database.Put(key, value)
	db.written.Store(true)
	return err
}

func (db *blockingReportDB) Close() error {
	db.closed.Store(true)
	if !db.written.Load() {
		return errors.New("database closed before report write completed")
	}
	return db.Database.Close()
}

// TestBackendStopDrainsReportsBeforeClosingDatabase checks that shutdown waits
// for an in-flight report write before closing its database.
func TestBackendStopDrainsReportsBeforeClosingDatabase(t *testing.T) {
	stack := newTestNode(t, "pebble")
	cfg, slave := newTestConfig(t, new(testShard))
	seedRoot(t, stack, cfg)
	b := newTestBackend(t, stack, cfg)
	db := &blockingReportDB{Database: b.db, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	// Release a blocked Put before node cleanup if the test fails.
	t.Cleanup(func() { releaseOnce.Do(func() { close(db.release) }) })
	b.chain.Close()
	chain, err := root.New(b.ctx, cfg.Genesis.Config, b.cluster, db, b.engine)
	if err != nil {
		t.Fatal(err)
	}
	b.chain, b.db = chain, db
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	waitBackendReady(t, b)
	header := &coretypes.Header{
		Number: big.NewInt(1), Difficulty: new(big.Int), UncleHash: coretypes.EmptyUncleHash,
		Extra: (&coretypes.QKCExtra{ChainID: cfg.Genesis.Config.ChainIDs[0]}).Encode(),
	}
	reportDone := make(chan error, 1)
	go func() { reportDone <- slave.SendShardBlockHeaderToMaster(testContext(t), header, uint256.NewInt(1)) }()
	select {
	case <-db.entered:
	case <-testContext(t).Done():
		t.Fatal("report did not reach DB")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- stack.Close() }()
	select {
	case <-b.ctx.Done():
	case <-testContext(t).Done():
		t.Fatal("Stop did not cancel backend")
	}
	if db.closed.Load() {
		t.Fatal("database closed while report was writing")
	}
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before report drained: %v", err)
	case <-time.After(50 * time.Millisecond):
		// Network teardown runs on real goroutines. Leave the write blocked
		// while Stop has time to reach draining; Close also checks completion.
	}
	releaseOnce.Do(func() { close(db.release) })
	select {
	case err := <-stopDone:
		if err != nil || !db.closed.Load() {
			t.Fatalf("database not closed after drain: %v", err)
		}
	case <-testContext(t).Done():
		t.Fatal("report handler was not drained")
	}
	select {
	case <-reportDone:
	case <-testContext(t).Done():
		t.Fatal("report caller remained blocked")
	}
}

// A restart may omit the root seed but must preserve an advanced persisted head.
func TestBackendRestartWithoutRootGenesis(t *testing.T) {
	for _, dbEngine := range []string{"pebble", "leveldb"} {
		t.Run(dbEngine, func(t *testing.T) {
			stack := newTestNode(t, dbEngine)
			cfg, _ := newTestConfig(t, new(testShard))
			genesis := seedRoot(t, stack, cfg)
			db, err := stack.OpenDatabaseWithOptions(stack.Config().DataDir, testDatabaseOptions())
			if err != nil {
				t.Fatal(err)
			}
			head := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
				Number: 1, ParentHash: genesis.Hash(), Time: 2,
				Difficulty: *uint256.NewInt(100), TotalDifficulty: *uint256.NewInt(200),
			}, nil, nil)
			confirmed, err := qkcrawdb.ReadRootConfirmedShardBlocks(db, genesis.Hash())
			if err != nil {
				t.Fatal(err)
			}
			qkcrawdb.WriteRootBlock(db, head)
			for _, err := range []error{
				qkcrawdb.WriteRootCanonicalHash(db, head.Hash(), 1),
				qkcrawdb.WriteRootHeadBlockHash(db, head.Hash()),
				qkcrawdb.WriteRootConfirmedShardBlocks(db, head.Hash(), confirmed),
				db.Close(),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg.Genesis = nil
			b := newTestBackend(t, stack, cfg)
			if got := b.RootChain().RootTip().Hash(); got != head.Hash() {
				t.Fatalf("recovered tip %s, want %s", got, head.Hash())
			}
			if err := stack.Start(); err != nil {
				t.Fatal(err)
			}
			waitBackendReady(t, b)
		})
	}
}

func TestBackendRejectsGenesisMismatch(t *testing.T) {
	for _, mismatch := range []string{"root block", "root rules", "shard genesis"} {
		t.Run(mismatch, func(t *testing.T) {
			stack := newTestNode(t, "pebble")
			shard := new(testShard)
			cfg, _ := newTestConfig(t, shard)
			genesis := seedRoot(t, stack, cfg)
			var restore func()
			switch mismatch {
			case "root block":
				cfg.Genesis.Timestamp++
				restore = func() { cfg.Genesis.Timestamp-- }
			case "root rules":
				cfg.Genesis.Config.PoSW.AdjustmentFactor++
				restore = func() { cfg.Genesis.Config.PoSW.AdjustmentFactor-- }
			case "shard genesis":
				cfg.Genesis = nil
				cfg.Shards[0].Genesis.Coinbase = common.Address{1}
				restore = func() { cfg.Shards[0].Genesis.Coinbase = common.Address{} }
			}
			if b, err := New(stack, cfg, testDatabaseOptions()); err == nil || b != nil {
				t.Fatalf("accepted %s mismatch: %v", mismatch, err)
			}
			if shard.inits.Load() != 0 {
				t.Fatal("failed constructor initialized shards")
			}
			restore()
			// Reopening proves failed construction released its database and
			// did not register a lifecycle or change the persisted head.
			b := newTestBackend(t, stack, cfg)
			if b.RootChain().RootTip().Hash() != genesis.Hash() {
				t.Fatal("failed constructor changed persisted head")
			}
			if err := stack.Start(); err != nil {
				t.Fatal(err)
			}
			waitBackendReady(t, b)
		})
	}
}
