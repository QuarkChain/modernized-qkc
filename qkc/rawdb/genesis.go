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

package rawdb

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
)

// ReadRootChainConfig reads root consensus rules independently of the genesis
// hash. Shard configurations use the native geth chain configuration accessors.
func ReadRootChainConfig(db ethdb.KeyValueReader) ([]byte, error) {
	has, err := db.Has(rootChainConfigKey)
	if err != nil || !has {
		return nil, err
	}
	return db.Get(rootChainConfigKey)
}

func WriteRootChainConfig(db ethdb.KeyValueWriter, data []byte) error {
	return db.Put(rootChainConfigKey, data)
}

func ReadRootGenesisHash(db ethdb.KeyValueReader) (common.Hash, error) {
	has, err := db.Has(rootGenesisHashKey)
	if err != nil || !has {
		return common.Hash{}, err
	}
	data, err := db.Get(rootGenesisHashKey)
	if err != nil {
		return common.Hash{}, err
	}
	if len(data) != common.HashLength {
		return common.Hash{}, fmt.Errorf("invalid root genesis hash length %d", len(data))
	}
	return common.BytesToHash(data), nil
}

func WriteRootGenesisHash(db ethdb.KeyValueWriter, hash common.Hash) error {
	return db.Put(rootGenesisHashKey, hash[:])
}
