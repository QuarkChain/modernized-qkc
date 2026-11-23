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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
)

func TestRootGenesisSetupPreservesAdvancedHead(t *testing.T) {
	f := newRootEnv(t)
	r1 := f.block(f.genesis, 1)
	f.store(r1, true)
	for _, seed := range []*types.RootBlock{f.genesis, nil} {
		config, hash, err := SetupRootGenesisBlock(f.db, seed, f.config)
		if err != nil || hash != f.genesis.Hash() || !config.Equal(f.config) {
			t.Fatalf("setup returned config %v, genesis %x, error %v", config, hash, err)
		}
		if got := f.open().RootTip().Hash(); got != r1.Hash() {
			t.Fatalf("reopened tip = %x, want %x", got, r1.Hash())
		}
	}
	// An adjustment rule is not committed by R0, but still determines the chain.
	f.config.PoSW.AdjustmentFactor++
	if _, _, err := SetupRootGenesisBlock(f.db, f.genesis, f.config); err == nil {
		t.Fatal("accepted incompatible PoSW configuration")
	}
	f.config.PoSW.AdjustmentFactor--
	header := f.genesis.Header()
	header.Time++
	different := types.NewRootBlock(header, f.genesis.ShardBlockHeaders(), nil)
	if _, _, err := SetupRootGenesisBlock(f.db, different, f.config); err == nil {
		t.Fatal("accepted a different R0")
	}
	if got := f.open().RootTip().Hash(); got != r1.Hash() {
		t.Fatalf("rejected setup changed tip to %x", got)
	}
}

func TestRootGenesisSetupMissingRecords(t *testing.T) {
	f := newRootEnv(t)
	records := make(map[string][]byte)
	it := f.db.NewIterator(nil, nil)
	for it.Next() {
		records[string(it.Key())] = common.CopyBytes(it.Value())
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	it.Release()
	// Missing canonical(0) selects initialization. Other missing records in an
	// initialized database must fail without repairs.
	for key := range records {
		if err := f.db.Delete([]byte(key)); err != nil {
			t.Fatal(err)
		}
		hash, err := qkcrawdb.ReadRootGenesisHash(f.db)
		if err != nil {
			t.Fatal(err)
		}
		if hash == (common.Hash{}) {
			if _, hash, err := SetupRootGenesisBlock(f.db, f.genesis, f.config); err != nil || hash != f.genesis.Hash() {
				t.Fatalf("initialization without canonical genesis returned %x, error %v", hash, err)
			}
			if hash, err := qkcrawdb.ReadRootGenesisHash(f.db); err != nil || hash != f.genesis.Hash() {
				t.Fatalf("canonical genesis after initialization = %x, error %v", hash, err)
			}
			continue
		}
		if _, _, err := SetupRootGenesisBlock(f.db, f.genesis, f.config); err == nil {
			t.Fatalf("accepted missing record %x", key)
		}
		if has, err := f.db.Has([]byte(key)); err != nil || has {
			t.Fatalf("missing record %x recreated: %t, %v", key, has, err)
		}
		if err := f.db.Put([]byte(key), records[key]); err != nil {
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
	if _, _, err := SetupRootGenesisBlock(f.db, nil, nil); err == nil {
		t.Fatal("accepted stale R0 confirmations")
	}
	if err := qkcrawdb.WriteRootConfirmedShardBlocks(f.db, f.genesis.Hash(), f.confirmed); err != nil {
		t.Fatal(err)
	}
	qkcrawdb.WriteRootBlock(f.db, f.genesis.WithBody(nil, nil))
	if _, _, err := SetupRootGenesisBlock(f.db, nil, nil); err == nil {
		t.Fatal("accepted invalid stored R0 body")
	}
}

func TestRootGenesisSetupRejectsInvalidSeed(t *testing.T) {
	f := newRootEnv(t)
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
			if _, _, err := SetupRootGenesisBlock(db, seed, f.config); err == nil {
				t.Fatal("accepted malformed root genesis")
			}
			if hash, err := qkcrawdb.ReadRootGenesisHash(db); err != nil || hash != (common.Hash{}) {
				t.Fatalf("rejected seed left canonical genesis %x, error %v", hash, err)
			}
		})
	}
}

type genesisWriteFailure struct {
	ethdb.Database
	err error
}

func (db *genesisWriteFailure) NewBatch() ethdb.Batch {
	return &failedGenesisBatch{Batch: db.Database.NewBatch(), err: db.err}
}

type failedGenesisBatch struct {
	ethdb.Batch
	err error
}

func (b *failedGenesisBatch) Write() error { return b.err }

func TestRootGenesisSetupAtomic(t *testing.T) {
	f := newRootEnv(t)
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	if _, _, err := SetupRootGenesisBlock(db, nil, nil); err == nil {
		t.Fatal("accepted empty database without genesis")
	}
	failure := &genesisWriteFailure{db, errors.New("batch write failed")}
	if _, _, err := SetupRootGenesisBlock(failure, f.genesis, f.config); !errors.Is(err, failure.err) {
		t.Fatalf("setup error %v, want %v", err, failure.err)
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	if it.Next() {
		t.Fatalf("failed batch left record %x", it.Key())
	}
	if _, _, err := SetupRootGenesisBlock(db, f.genesis, f.config); err != nil {
		t.Fatalf("retry: %v", err)
	}
}
