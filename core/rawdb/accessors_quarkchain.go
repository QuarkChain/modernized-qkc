// Copyright 2026 The quarkchain Authors
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

package rawdb

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

func xShardKey(prefix string, shard uint32) []byte {
	return binary.BigEndian.AppendUint32([]byte(prefix), shard)
}

// ReadXShardMessages returns the stored Outbox logs of a source block. A nil
// result means the block has not been read; an empty list means no matching logs.
func ReadXShardMessages(db ethdb.KeyValueReader, shard uint32, hash common.Hash) ([]types.Log, error) {
	key := append(xShardKey("qkc-xshard-logs-", shard), hash[:]...)
	has, err := db.Has(key)
	if err != nil || !has {
		return nil, err
	}
	data, err := db.Get(key)
	if err != nil {
		return nil, err
	}
	var logs []types.Log
	err = json.Unmarshal(data, &logs)
	return logs, err
}

// WriteXShardMessages stores logs with their source block and transaction metadata.
func WriteXShardMessages(db ethdb.KeyValueWriter, shard uint32, hash common.Hash, logs []types.Log) error {
	data, err := json.Marshal(logs)
	if err != nil {
		return err
	}
	return db.Put(append(xShardKey("qkc-xshard-logs-", shard), hash[:]...), data)
}

// ReadXShardDownloadCursor returns the last source block whose messages were stored
// locally. It tracks download progress, not consumption. A zero hash means no progress.
func ReadXShardDownloadCursor(db ethdb.KeyValueReader, shard uint32) (uint64, common.Hash, error) {
	key := xShardKey("qkc-xshard-cursor-", shard)
	has, err := db.Has(key)
	if err != nil || !has {
		return 0, common.Hash{}, err
	}
	data, err := db.Get(key)
	if err != nil {
		return 0, common.Hash{}, err
	}
	if len(data) != 8+common.HashLength {
		return 0, common.Hash{}, fmt.Errorf("invalid xshard download cursor length %d", len(data))
	}
	return binary.BigEndian.Uint64(data[:8]), common.BytesToHash(data[8:]), nil
}

// WriteXShardDownloadCursor must be committed in the same batch as the corresponding logs.
func WriteXShardDownloadCursor(db ethdb.KeyValueWriter, shard uint32, number uint64, hash common.Hash) error {
	data := binary.BigEndian.AppendUint64(nil, number)
	return db.Put(xShardKey("qkc-xshard-cursor-", shard), append(data, hash[:]...))
}
