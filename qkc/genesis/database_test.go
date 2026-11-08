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

package genesis

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
)

var errInterruptedInit = errors.New("interrupted initialization")

type interruptedDB struct {
	ethdb.Database
	sync   bool
	failed bool
}

type interruptedBatch struct {
	ethdb.Batch
	db *interruptedDB
}

func (db *interruptedDB) NewBatch() ethdb.Batch {
	return &interruptedBatch{Batch: db.Database.NewBatch(), db: db}
}

func (b *interruptedBatch) Write() error {
	if !b.db.sync && !b.db.failed {
		b.db.failed = true
		return errInterruptedInit
	}
	return b.Batch.Write()
}

func (db *interruptedDB) SyncKeyValue() error {
	if db.sync && !db.failed {
		db.failed = true
		return errInterruptedInit
	}
	return db.Database.SyncKeyValue()
}

func TestRootDatabaseRecovery(t *testing.T) {
	bundle, err := Load(exampleInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, failSync := range []bool{false, true} {
		disk := rawdb.NewMemoryDatabase()
		db := &interruptedDB{Database: disk, sync: failSync}
		if _, err := bundle.InitRootDatabase(db); !errors.Is(err, errInterruptedInit) {
			t.Fatalf("expected write or sync interruption: %v", err)
		}
		if _, err := bundle.InitRootDatabase(db); err != nil {
			t.Fatalf("resume root initialization: %v", err)
		}
		if hash, err := bundle.InitRootDatabase(db); err != nil || hash != bundle.Root.Hash() {
			t.Fatalf("repeat root initialization: %v", err)
		}
		config, err := qkcrawdb.ReadRootChainConfig(db)
		if err != nil || len(config) == 0 || bytes.Contains(config, []byte("shards")) {
			t.Fatal("root configuration not stored independently")
		}
		if rawdb.ReadCanonicalHash(db, 0) != (common.Hash{}) {
			t.Fatal("root database gained an EVM chain")
		}
		disk.Close()
	}
}

func TestRootDatabaseCompatibility(t *testing.T) {
	bundle, err := Load(exampleInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	if _, err := bundle.InitRootDatabase(db); err != nil {
		t.Fatal(err)
	}
	bundle.RootConfig.PoSW.CoinbaseAmount.SetInt64(1)
	if _, err := bundle.InitRootDatabase(db); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("changed root PoSW rules accepted: %v", err)
	}
	if hash, err := qkcrawdb.ReadRootGenesisHash(db); err != nil || hash != bundle.Root.Hash() {
		t.Fatal("incompatible initialization changed root genesis")
	}
	other := rawdb.NewMemoryDatabase()
	defer other.Close()
	if err := other.Put([]byte("foreign"), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.InitRootDatabase(other); err == nil {
		t.Fatal("unrecognized root database accepted")
	}
}
