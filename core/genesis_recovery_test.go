// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

type genesisFailureDB struct {
	ethdb.Database
	failed bool
}

type genesisFailureBatch struct {
	ethdb.Batch
	db      *genesisFailureDB
	genesis bool
}

var errGenesisBatchInterrupted = errors.New("interrupted genesis block batch")

func (db *genesisFailureDB) NewBatch() ethdb.Batch {
	return &genesisFailureBatch{Batch: db.Database.NewBatch(), db: db}
}

func (db *genesisFailureDB) NewBatchWithSize(size int) ethdb.Batch {
	return &genesisFailureBatch{Batch: db.Database.NewBatchWithSize(size), db: db}
}

func (b *genesisFailureBatch) Put(key, value []byte) error {
	if bytes.HasPrefix(key, []byte("ethereum-genesis-")) {
		b.genesis = true
	}
	return b.Batch.Put(key, value)
}

func (b *genesisFailureBatch) Write() error {
	if b.genesis && !b.db.failed {
		b.db.failed = true
		return errGenesisBatchInterrupted
	}
	return b.Batch.Write()
}

func TestSetupGenesisBlockInterruptedCommit(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			disk := rawdb.NewMemoryDatabase()
			defer disk.Close()
			db := &genesisFailureDB{Database: disk}
			spec := DeveloperGenesisBlock(12000000, nil)
			initialize := func() (err error) {
				config := *triedb.HashDefaults
				if scheme == rawdb.PathScheme {
					config = triedb.Config{PathDB: pathdb.Defaults}
				}
				trie := triedb.NewDatabase(db, &config)
				defer func() { err = errors.Join(err, trie.Close()) }()
				_, _, compatErr, err := SetupGenesisBlock(db, trie, spec)
				if compatErr != nil {
					return compatErr
				}
				return err
			}
			if err := initialize(); !errors.Is(err, errGenesisBatchInterrupted) {
				t.Fatalf("expected interrupted block batch, got %v", err)
			}
			if rawdb.ReadCanonicalHash(db, 0) != (common.Hash{}) {
				t.Fatal("failed genesis block batch was committed")
			}
			if err := initialize(); err != nil {
				t.Fatalf("resume genesis initialization: %v", err)
			}
			if err := initialize(); err != nil {
				t.Fatalf("repeat genesis initialization: %v", err)
			}
			if rawdb.ReadCanonicalHash(db, 0) != spec.ToBlock().Hash() {
				t.Fatal("recovered genesis differs from specification")
			}
		})
	}
}
