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
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
)

// InitRootDatabase persists root genesis and its consensus configuration in one
// atomic batch. The root hash anchors genesis, but does not cover all PoSW rules.
// Matching data is left untouched, so retries preserve any later root blocks.
// The caller owns the database and its lock. Shards use core.SetupGenesisBlock.
func (b *Bundle) InitRootDatabase(db ethdb.Database) (common.Hash, error) {
	if b == nil || b.Root == nil || b.RootConfig == nil {
		return common.Hash{}, errors.New("validated root genesis and configuration are required")
	}
	hash := b.Root.Hash()
	if rawdb.ReadCanonicalHash(db, 0) != (common.Hash{}) {
		return common.Hash{}, errors.New("root database contains an EVM chain")
	}
	stored, err := qkcrawdb.ReadRootGenesisHash(db)
	if err != nil {
		return common.Hash{}, err
	}
	config, err := json.Marshal(b.RootConfig)
	if err != nil {
		return common.Hash{}, err
	}
	block, err := b.Root.MarshalSSZ()
	if err != nil {
		return common.Hash{}, err
	}
	if stored != (common.Hash{}) {
		storedConfig, err := qkcrawdb.ReadRootChainConfig(db)
		if err != nil {
			return common.Hash{}, err
		}
		if stored != hash || !bytes.Equal(storedConfig, config) {
			return common.Hash{}, errors.New("incompatible root genesis or consensus configuration")
		}
		if !bytes.Equal(qkcrawdb.ReadRootBlockSSZ(db, hash), block) {
			return common.Hash{}, errors.New("stored root genesis block is missing or inconsistent")
		}
		return hash, db.SyncKeyValue()
	}
	iterator := db.NewIterator(nil, nil)
	hasData := iterator.Next()
	err = iterator.Error()
	iterator.Release()
	if err != nil {
		return common.Hash{}, err
	}
	ancients, _ := db.Ancients()
	if hasData || ancients != 0 {
		return common.Hash{}, errors.New("database contains data without root genesis; use a new data directory")
	}
	batch := db.NewBatch()
	defer batch.Close()
	qkcrawdb.WriteRootBlock(batch, b.Root)
	if err := qkcrawdb.WriteRootChainConfig(batch, config); err != nil {
		return common.Hash{}, err
	}
	if err := qkcrawdb.WriteRootGenesisHash(batch, hash); err != nil {
		return common.Hash{}, err
	}
	if err := batch.Write(); err != nil {
		return common.Hash{}, err
	}
	return hash, db.SyncKeyValue()
}
