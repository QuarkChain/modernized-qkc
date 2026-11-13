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

package eth

import (
	"context"
	"errors"
	"maps"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/holiman/uint256"
)

func qkcBackendConfig() ethconfig.Config {
	config := ethconfig.Defaults
	chainConfig := params.NewTestQuarkChainConfig(10001)
	alloc := core.SystemContractAllocs()
	maps.Copy(alloc, core.PoSWStakingContractAllocs(chainConfig))
	alloc[address] = types.Account{Balance: new(big.Int).Set(funds)}
	config.Genesis = &core.Genesis{
		Config: chainConfig, Alloc: alloc, GasLimit: 12_000_000,
		Difficulty: new(big.Int), Mixhash: common.BigToHash(common.Big1),
	}
	config.Genesis.ExtraData = (&types.QKCExtra{
		ChainID: chainConfig.ChainID, Cursor: types.XShardCursor{RootBlockHeight: 1},
		XShardGasLimit: config.Genesis.GasLimit / params.XShardGasLimitDivisor,
	}).Encode()
	config.StateScheme, config.SyncMode = rawdb.HashScheme, ethconfig.FullSync
	config.DatabaseCache, config.DatabaseHandles = 16, 16
	config.TrieCleanCache, config.TrieDirtyCache, config.SnapshotCache = 16, 16, 0
	config.TxPool.Journal = ""
	config.Miner.PendingFeeRecipient = common.Address{0xaa}
	return config
}

func qkcBackendNode(t *testing.T, datadir string) *node.Node {
	t.Helper()
	if datadir == "" {
		datadir = t.TempDir()
	}
	stack, err := node.New(&node.Config{
		Name: "qkc-backend-test", DataDir: datadir,
		P2P: p2p.Config{NoDiscovery: true, NoDial: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stack.Close() })
	return stack
}

func startQKCBackend(t *testing.T, stack *node.Node, config *ethconfig.Config) *Ethereum {
	t.Helper()
	prepareQKCGenesis(t, stack, config)
	backend, err := New(stack, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	return backend
}

// Initialize genesis as the deployment setup does, before Ethereum opens its DB.
func prepareQKCGenesis(t *testing.T, stack *node.Node, config *ethconfig.Config) {
	t.Helper()
	if config.Genesis == nil {
		return
	}
	db, err := stack.OpenDatabaseWithOptions("chaindata", node.DatabaseOptions{Cache: 16, Handles: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if rawdb.ReadCanonicalHash(db, 0) != (common.Hash{}) {
		return
	}
	trieConfig := triedb.HashDefaults
	if config.StateScheme == rawdb.PathScheme {
		trieConfig = &triedb.Config{PathDB: pathdb.Defaults}
	}
	tdb := triedb.NewDatabase(db, trieConfig)
	defer tdb.Close()
	if _, err := config.Genesis.Commit(db, tdb, nil); err != nil {
		t.Fatal(err)
	}
}

func initQKCBackend(t *testing.T, backend *Ethereum) *qkctypes.RootBlock {
	t.Helper()
	root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1),
	}, qkctypes.ShardBlockHeaders{backend.BlockChain().Genesis().Header()}, nil)
	if err := backend.Shard().Init(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// Build a transaction from Ethereum's public pool to check that ShardChain uses
// the same chain, PoSW engine and builder. Starting Node must not fake master Init.
func TestQKCBackendIntegration(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			config := qkcBackendConfig()
			config.StateScheme = scheme
			stack := qkcBackendNode(t, "")
			backend := startQKCBackend(t, stack, &config)
			if _, ok := backend.Engine().(*posw.PoSW); !ok || backend.BlockChain().Engine() != backend.Engine() {
				t.Fatal("BlockChain must use the Ethereum-owned PoSW engine")
			}
			if backend.Shard().CurrentBlock() != backend.BlockChain().CurrentBlock() {
				t.Fatal("Shard uses a different BlockChain")
			}
			if backend.Shard().RootTip() != (common.Hash{}) {
				t.Fatal("mock service initialized the shard")
			}
			if _, err := backend.PoWMiner().GetWork(context.Background(), address); !errors.Is(err, shard.ErrRootChainUninitialized) {
				t.Fatalf("work before Init: %v", err)
			}
			if len(backend.Protocols()) != 0 || backend.BlobTxPool() != nil || backend.BlobCache() != nil {
				t.Fatal("native network protocols and blob pool must be disabled")
			}
			if backend.Downloader() == nil || backend.BlobFetcher() == nil {
				t.Fatal("existing handler components must remain available")
			}
			backend.APIBackend.SyncProgress(context.Background())
			if err := backend.APIBackend.SetHead(0); err == nil {
				t.Fatal("debug_setHead must not bypass ShardChain")
			}
			client := stack.Attach()
			defer client.Close()
			var chainID hexutil.Big
			if err := client.Call(&chainID, "eth_chainId"); err != nil || (*big.Int)(&chainID).Cmp(config.Genesis.Config.ChainID) != 0 {
				t.Fatalf("existing eth API: chain ID %v, error %v", chainID, err)
			}
			if err := client.Call(nil, "miner_setGasPrice", hexutil.Uint64(1)); err != nil {
				t.Fatalf("existing MinerAPI: %v", err)
			}
			initQKCBackend(t, backend)
			tx := types.MustSignNewTx(key, types.LatestSigner(config.Genesis.Config), &types.LegacyTx{
				Gas: params.TxGas, GasPrice: big.NewInt(2 * params.InitialBaseFee), To: &address, Value: big.NewInt(1),
			})
			if err := backend.TxPool().Add([]*types.Transaction{tx}, true)[0]; err != nil {
				t.Fatal(err)
			}
			block, _, err := backend.Shard().CreateBlockToMine(context.Background(), config.Miner.PendingFeeRecipient)
			if err != nil {
				t.Fatal(err)
			}
			if len(block.Transactions()) != 1 || block.Transactions()[0].Hash() != tx.Hash() {
				t.Fatal("shared builder did not use the shared transaction pool")
			}
		})
	}
}

// Exercise configured simulated consensus through the backend-owned chain and
// builder.
func TestQKCBackendSimulatedMining(t *testing.T) {
	config := qkcBackendConfig()
	config.Genesis.Config.QuarkChain.ConsensusType = params.ConsensusSimulate
	backend := startQKCBackend(t, qkcBackendNode(t, ""), &config)
	pow, ok := backend.Engine().(*posw.PoSW)
	if !ok || !pow.IsSimulate() {
		t.Fatalf("consensus engine %T, want simulated PoSW", backend.Engine())
	}
	chain := backend.BlockChain()
	if chain.Engine() != pow {
		t.Fatal("BlockChain must use the Ethereum-owned PoSW engine")
	}
	initQKCBackend(t, backend)
	coinbase := common.Address{0x42}
	// Local mining does not yet select a searcher from ConsensusType.
	backend.powMiner = shard.NewPoWMiner(backend.Shard(), shard.PoWMinerConfig{
		Coinbase: coinbase, Search: shard.NewSimulatedSearch(100 * time.Millisecond),
	})
	m := backend.PoWMiner()
	t.Cleanup(m.StopLocalMining)
	if err := m.StartLocalMining(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); chain.CurrentBlock().Number.Uint64() < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("head #%d did not reach #3", chain.CurrentBlock().Number)
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.StopLocalMining()
	head := chain.CurrentBlock()
	parent := chain.Genesis()
	for n := uint64(1); n <= head.Number.Uint64(); n++ {
		block := chain.GetBlockByNumber(n)
		if block == nil || block.NumberU64() != n || block.ParentHash() != parent.Hash() {
			t.Fatalf("block #%d does not extend the canonical chain", n)
		}
		if block.Coinbase() != coinbase || block.Time() <= parent.Time() || block.Time() > uint64(time.Now().Unix()) {
			t.Fatalf("unexpected block #%d: coinbase %x, time %d after %d", n, block.Coinbase(), block.Time(), parent.Time())
		}
		if err := pow.VerifyHeader(chain, block.Header()); err != nil {
			t.Fatalf("block #%d header verification: %v", n, err)
		}
		if err := pow.VerifySeal(chain, block.Header()); err != nil {
			t.Fatalf("block #%d seal verification: %v", n, err)
		}
		if committed, err := qkcrawdb.IsShardBlockCommitted(backend.ChainDb(), block.Hash()); err != nil || !committed {
			t.Fatalf("block #%d delivery not committed: %v", n, err)
		}
		parent = block
	}
	if head.Hash() != parent.Hash() {
		t.Fatal("head is not the last validated block")
	}
	t.Logf("Validated %d consecutive canonical blocks with simulated consensus and committed delivery", head.Number.Uint64())
}

type qkcWaitingSearch struct {
	started, cancelled, release chan struct{}
}

func (s *qkcWaitingSearch) Search(ctx context.Context, _ *shard.MiningWork) (types.BlockNonce, common.Hash, bool) {
	close(s.started)
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	return types.BlockNonce{}, common.Hash{}, false
}

// A cancelled search can still be releasing resources. Node shutdown must wait
// for that real mining task to return before closing its shared database.
func TestQKCBackendStopWaitsForMining(t *testing.T) {
	config := qkcBackendConfig()
	stack := qkcBackendNode(t, "")
	backend := startQKCBackend(t, stack, &config)
	initQKCBackend(t, backend)
	search := &qkcWaitingSearch{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	release := sync.OnceFunc(func() { close(search.release) })
	defer release()
	if err := backend.PoWMiner().StartLocalMining(); err == nil {
		t.Fatal("local mining started without a searcher")
	}
	// The default miner is idle; construct the test miner with its searcher.
	backend.powMiner = shard.NewPoWMiner(backend.Shard(), shard.PoWMinerConfig{
		Coinbase: config.Miner.PendingFeeRecipient, Search: search,
	})
	if err := backend.PoWMiner().StartLocalMining(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-search.started:
	case <-time.After(5 * time.Second):
		t.Fatal("local mining did not start")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- stack.Close() }()
	select {
	case <-search.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel mining")
	}
	if err := backend.ChainDb().Put([]byte("in-flight"), []byte{1}); err != nil {
		t.Fatalf("database closed before mining returned: %v", err)
	}
	select {
	case err := <-stopped:
		t.Fatalf("shutdown returned before mining completed: %v", err)
	default:
	}
	release()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

// Restart without a genesis file must create PoSW from the persisted genesis,
// while master still supplies Init explicitly.
func TestQKCBackendRestart(t *testing.T) {
	config := qkcBackendConfig()
	datadir := t.TempDir()
	stack := qkcBackendNode(t, datadir)
	root := initQKCBackend(t, startQKCBackend(t, stack, &config))
	if err := stack.Close(); err != nil {
		t.Fatal(err)
	}
	config.Genesis = nil
	backend := startQKCBackend(t, qkcBackendNode(t, datadir), &config)
	if backend.Shard().RootTip() != (common.Hash{}) {
		t.Fatal("restart bypassed explicit Init")
	}
	if err := backend.Shard().Init(root); err != nil {
		t.Fatal(err)
	}
}

// QKC forces full sync even when the chain config is loaded only from DB.
func TestQKCBackendSyncMode(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "snap sync"
		if restart {
			name = "snap sync without genesis file"
		}
		t.Run(name, func(t *testing.T) {
			config := qkcBackendConfig()
			stack := qkcBackendNode(t, t.TempDir())
			config.SyncMode = ethconfig.SnapSync
			if restart {
				prepareQKCGenesis(t, stack, &config)
				config.Genesis = nil
			}
			backend := startQKCBackend(t, stack, &config)
			if config.SyncMode != ethconfig.FullSync || backend.Downloader().ConfigSyncMode() != ethconfig.FullSync {
				t.Fatal("QKC did not force full sync")
			}
		})
	}
}

// Startup errors are returned to cmd/geth, which exits rather than retrying New.
func TestQKCBackendConstructionErrors(t *testing.T) {
	t.Run("invalid genesis", func(t *testing.T) {
		config := qkcBackendConfig()
		config.Genesis.ExtraData = []byte{0}
		stack := qkcBackendNode(t, t.TempDir())
		if backend, err := New(stack, &config); err == nil || backend != nil {
			t.Fatalf("invalid genesis accepted: %v", err)
		}
	})
	t.Run("invalid tracer", func(t *testing.T) {
		config := qkcBackendConfig()
		stack := qkcBackendNode(t, t.TempDir())
		prepareQKCGenesis(t, stack, &config)
		config.VMTrace = "missing-qkc-test-tracer"
		if backend, err := New(stack, &config); err == nil || backend != nil {
			t.Fatalf("invalid tracer accepted: %v", err)
		}
	})
}
