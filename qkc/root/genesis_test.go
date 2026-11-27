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
	"errors"
	"maps"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
)

func newRootGenesisInputs(chainIDs ...int64) (*RootGenesis, []*core.Genesis) {
	config := params.NewTestQuarkChainConfig(chainIDs[0], chainIDs...).QuarkChain
	config.ConsensusType = params.ConsensusEthash
	seed := &RootGenesis{Config: config, Timestamp: 1, Difficulty: big.NewInt(100)}
	const gasLimit = 1_000_000
	shards := make([]*core.Genesis, 0, len(chainIDs))
	for _, id := range chainIDs {
		shardConfig := params.NewTestQuarkChainConfig(id, chainIDs...)
		shards = append(shards, &core.Genesis{
			Config: shardConfig, Timestamp: 1, GasLimit: gasLimit, Difficulty: new(big.Int),
			Mixhash: common.BigToHash(shardConfig.QuarkChain.PoSW.MinimumDifficulty),
			Alloc:   core.PoSWStakingContractAllocs(shardConfig),
			ExtraData: (&coretypes.QKCExtra{
				ChainID:        shardConfig.ChainID,
				Cursor:         coretypes.XShardCursor{RootBlockHeight: 1},
				XShardGasLimit: gasLimit / params.XShardGasLimitDivisor,
			}).Encode(),
		})
	}
	return seed, shards
}

func newRootGenesisEnv(t *testing.T) (*rootEnv, *RootGenesis, []*core.Genesis) {
	t.Helper()
	seed, shards := newRootGenesisInputs(10001, 10002)
	config := seed.Config
	db := &rootDB{Database: rawdb.NewMemoryDatabase()}
	t.Cleanup(func() { db.Close() })
	_, hash, err := SetupRootGenesisBlock(db, seed, shards)
	if err != nil {
		t.Fatal(err)
	}
	genesis := qkcrawdb.ReadRootBlock(db, hash)
	if genesis == nil {
		t.Fatal("missing initialized genesis block")
	}
	confirmed := make(map[string]common.Hash, len(shards))
	for i, id := range config.ChainIDs {
		confirmed[id.String()] = genesis.ShardBlockHeaders()[i].Hash()
	}
	engine, err := posw.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return &rootEnv{
		t: t, config: config, db: db, client: new(rootClient), engine: engine,
		backendCtx: t.Context(), genesis: genesis, confirmed: confirmed,
	}, seed, shards
}

func TestRootGenesisSetupChainIDOrder(t *testing.T) {
	seed, shards := newRootGenesisInputs(10002, 10001)
	genesis, err := BuildRootGenesis(seed, shards)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	for _, input := range []*RootGenesis{seed, nil} {
		config, hash, err := SetupRootGenesisBlock(db, input, shards)
		if err != nil || hash != genesis.Hash() {
			t.Fatalf("setup returned genesis %x, want %x, error %v", hash, genesis.Hash(), err)
		}
		if ids := config.ChainIDs; len(ids) != 2 || ids[0].Int64() != 10002 || ids[1].Int64() != 10001 {
			t.Fatalf("chain IDs %v, want [10002 10001]", ids)
		}
	}
	confirmed, err := qkcrawdb.ReadRootConfirmedShardBlocks(db, genesis.Hash())
	if err != nil {
		t.Fatal(err)
	}
	for _, shard := range shards {
		id := shard.Config.ChainID.String()
		if got, want := confirmed[id], shard.ToBlock().Hash(); got != want {
			t.Errorf("chain %s confirmation = %x, want %x", id, got, want)
		}
	}
}

func TestRootGenesisSetupPreservesAdvancedHead(t *testing.T) {
	f, seed, shards := newRootGenesisEnv(t)
	r1 := f.block(f.genesis, 1)
	f.store(r1, true)
	for _, input := range []*RootGenesis{seed, nil} {
		config, hash, err := SetupRootGenesisBlock(f.db, input, shards)
		if err != nil || hash != f.genesis.Hash() || !config.Equal(f.config) {
			t.Fatalf("setup returned config %v, genesis %x, error %v", config, hash, err)
		}
		if got := f.open().RootTip().Hash(); got != r1.Hash() {
			t.Fatalf("reopened tip = %x, want %x", got, r1.Hash())
		}
	}
	// An adjustment rule is not committed by R0, but still determines the chain.
	f.config.PoSW.AdjustmentFactor++
	if _, _, err := SetupRootGenesisBlock(f.db, seed, shards); err == nil {
		t.Fatal("accepted incompatible PoSW configuration")
	}
	f.config.PoSW.AdjustmentFactor--
	different := *seed
	different.Timestamp++
	supplied, err := BuildRootGenesis(&different, shards)
	if err != nil {
		t.Fatal(err)
	}
	var mismatch *core.GenesisMismatchError
	if _, hash, err := SetupRootGenesisBlock(f.db, &different, shards); !errors.As(err, &mismatch) ||
		hash != f.genesis.Hash() || mismatch.Stored != hash || mismatch.New != supplied.Hash() {
		t.Fatalf("expected genesis mismatch for %x and %x, got hash %x, error %v", f.genesis.Hash(), supplied.Hash(), hash, err)
	}
	if got := f.open().RootTip().Hash(); got != r1.Hash() {
		t.Fatalf("rejected setup changed tip to %x", got)
	}
}

func TestRootGenesisSetupMissingCanonicalGenesis(t *testing.T) {
	f, seed, shards := newRootGenesisEnv(t)
	r1 := f.block(f.genesis, 1)
	f.store(r1, true)
	if err := qkcrawdb.DeleteRootCanonicalHash(f.db, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetupRootGenesisBlock(f.db, seed, shards); err == nil {
		t.Fatal("accepted missing canonical genesis with an existing root head")
	}
	if hash, err := qkcrawdb.ReadRootHeadBlockHash(f.db); err != nil || hash != r1.Hash() {
		t.Fatalf("root head = %x, want %x, error %v", hash, r1.Hash(), err)
	}
	if hash := qkcrawdb.ReadRootCanonicalHash(f.db, 0); hash != (common.Hash{}) {
		t.Fatalf("rejected setup recreated canonical genesis %x", hash)
	}
}

func TestRootGenesisSetupMissingRecords(t *testing.T) {
	f, seed, shards := newRootGenesisEnv(t)
	records := make(map[string][]byte)
	it := f.db.NewIterator(nil, nil)
	for it.Next() {
		records[string(it.Key())] = common.CopyBytes(it.Value())
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	it.Release()
	for key, value := range records {
		if err := f.db.Delete([]byte(key)); err != nil {
			t.Fatal(err)
		}
		hasHead, err := qkcrawdb.HasRootHeadBlockHash(f.db)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = SetupRootGenesisBlock(f.db, seed, shards)
		if !hasHead {
			// New validates the head after genesis verification.
			if err != nil {
				t.Fatalf("genesis verification depends on the root head: %v", err)
			}
			_, err = New(f.backendCtx, f.config, f.client, f.db, f.engine)
		}
		if err == nil {
			t.Fatalf("accepted missing record %x", key)
		}
		if has, err := f.db.Has([]byte(key)); err != nil || has {
			t.Fatalf("missing record %x recreated: %t, %v", key, has, err)
		}
		if err := f.db.Put([]byte(key), value); err != nil {
			t.Fatal(err)
		}
	}
	// Validate R0's snapshot and body even when the current head is healthy.
	f.store(f.block(f.genesis, 1), true)
	stale := maps.Clone(f.confirmed)
	stale[f.config.ChainIDs[0].String()] = common.Hash{1}
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, f.genesis.Hash(), stale); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetupRootGenesisBlock(f.db, nil, shards); err == nil {
		t.Fatal("accepted stale R0 confirmations")
	}
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, f.genesis.Hash(), f.confirmed); err != nil {
		t.Fatal(err)
	}
	qkcrawdb.WriteRootBlock(f.db, f.genesis.WithBody(nil, nil))
	if _, _, err := SetupRootGenesisBlock(f.db, nil, shards); err == nil {
		t.Fatal("accepted invalid stored R0 body")
	}
}

func TestRootGenesisSetupRequiresConsensusType(t *testing.T) {
	f, seed, shards := newRootGenesisEnv(t)
	config := *f.config
	config.ConsensusType = ""
	invalid := *seed
	invalid.Config = &config

	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	if _, _, err := SetupRootGenesisBlock(db, &invalid, shards); err == nil || !strings.Contains(err.Error(), "root ConsensusType is required") {
		t.Fatalf("initialization with no consensus type: %v", err)
	}
	if err := qkcrawdb.WriteRootChainConfig(f.db, &config); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetupRootGenesisBlock(f.db, nil, shards); err == nil || !strings.Contains(err.Error(), "root ConsensusType is required") {
		t.Fatalf("recovery with no stored consensus type: %v", err)
	}
}

func TestCommitRootGenesisRejectsMalformedBlock(t *testing.T) {
	f, _, _ := newRootGenesisEnv(t)
	g0 := coretypes.CopyHeader(f.genesis.ShardBlockHeaders()[0])
	extra, err := g0.QKCExtra()
	if err != nil {
		t.Fatal(err)
	}
	extra.Cursor.RootBlockHeight = 0
	g0.Extra = extra.Encode()
	badCursor := types.NewRootBlock(f.genesis.Header(), types.ShardBlockHeaders{g0, f.genesis.ShardBlockHeaders()[1]}, nil)
	reward := f.genesis.Header()
	reward.CoinbaseAmount.SetUint64(1)
	for name, seed := range map[string]*types.RootBlock{
		"shard cursor": badCursor,
		"root reward":  types.NewRootBlock(reward, f.genesis.ShardBlockHeaders(), nil),
		"shard order":  types.NewRootBlock(f.genesis.Header(), types.ShardBlockHeaders{f.genesis.ShardBlockHeaders()[1], f.genesis.ShardBlockHeaders()[0]}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			defer db.Close()
			if err := commitRootGenesis(db, seed, f.config); err == nil {
				t.Fatal("accepted malformed root genesis")
			}
			if hash := qkcrawdb.ReadRootCanonicalHash(db, 0); hash != (common.Hash{}) {
				t.Fatalf("rejected seed left canonical genesis %x", hash)
			}
		})
	}
}

func TestRootGenesisSetupAtomic(t *testing.T) {
	_, seed, shards := newRootGenesisEnv(t)
	db := &rootDB{Database: rawdb.NewMemoryDatabase()}
	defer db.Close()
	if _, _, err := SetupRootGenesisBlock(db, nil, shards); err == nil {
		t.Fatal("accepted empty database without genesis")
	}
	db.batchErr = errors.New("batch write failed")
	if _, _, err := SetupRootGenesisBlock(db, seed, shards); !errors.Is(err, db.batchErr) {
		t.Fatalf("setup error %v, want %v", err, db.batchErr)
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	if it.Next() {
		t.Fatalf("failed batch left record %x", it.Key())
	}
	db.batchErr = nil
	if _, _, err := SetupRootGenesisBlock(db, seed, shards); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
