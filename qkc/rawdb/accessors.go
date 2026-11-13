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
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// HasRootBlock verifies the existence of a root block corresponding to the hash.
func HasRootBlock(db ethdb.KeyValueReader, hash common.Hash) bool {
	has, err := db.Has(rootBlockKey(hash))
	return has && err == nil
}

// ReadRootBlockSSZ retrieves a root block in its raw SSZ database encoding.
func ReadRootBlockSSZ(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(rootBlockKey(hash))
	return data
}

// ReadRootBlock retrieves the root block corresponding to the hash.
func ReadRootBlock(db ethdb.KeyValueReader, hash common.Hash) *types.RootBlock {
	data := ReadRootBlockSSZ(db, hash)
	if len(data) == 0 {
		return nil
	}
	block := new(types.RootBlock)
	if err := block.UnmarshalSSZ(data); err != nil {
		log.Error("Invalid root block SSZ", "hash", hash, "err", err)
		return nil
	}
	return block
}

// ReadRootBlockHeader retrieves the header of the root block corresponding to
// the hash, without decoding the shard block headers it confirms.
func ReadRootBlockHeader(db ethdb.KeyValueReader, hash common.Hash) *types.RootBlockHeader {
	data := ReadRootBlockSSZ(db, hash)
	if len(data) == 0 {
		return nil
	}
	header, err := types.DecodeRootBlockHeaderFromBlockSSZ(data)
	if err != nil {
		log.Error("Invalid root block header SSZ", "hash", hash, "err", err)
		return nil
	}
	return header
}

// WriteRootBlock stores a root block into the database.
func WriteRootBlock(db ethdb.KeyValueWriter, block *types.RootBlock) {
	data, err := block.MarshalSSZ()
	if err != nil {
		log.Crit("Failed to SSZ encode root block", "err", err)
	}
	if err := db.Put(rootBlockKey(block.Hash()), data); err != nil {
		log.Crit("Failed to store root block", "err", err)
	}
}

// ReadRootHeadBlockHash retrieves the persisted canonical root head.
func ReadRootHeadBlockHash(db ethdb.KeyValueReader) (common.Hash, error) {
	data, err := db.Get(rootHeadBlockKey)
	return common.BytesToHash(data), err
}

// WriteRootHeadBlockHash stores the persisted canonical root head.
func WriteRootHeadBlockHash(db ethdb.KeyValueWriter, hash common.Hash) error {
	return db.Put(rootHeadBlockKey, hash.Bytes())
}

// ReadRootCommittingHash retrieves the root whose shard delivery is incomplete.
// An absent marker returns the zero hash without an error.
func ReadRootCommittingHash(db ethdb.KeyValueReader) (common.Hash, error) {
	// Get uses backend-specific not-found errors.
	exists, err := db.Has(rootCommittingKey)
	if err != nil || !exists {
		return common.Hash{}, err
	}
	data, err := db.Get(rootCommittingKey)
	return common.BytesToHash(data), err
}

// WriteRootCommittingHash stores the root awaiting shard delivery.
func WriteRootCommittingHash(db ethdb.KeyValueWriter, hash common.Hash) error {
	return db.Put(rootCommittingKey, hash.Bytes())
}

// DeleteRootCommittingHash clears the root awaiting shard delivery.
func DeleteRootCommittingHash(db ethdb.KeyValueWriter) error {
	return db.Delete(rootCommittingKey)
}

// ReadRootCanonicalHash retrieves the hash assigned to a canonical root height.
func ReadRootCanonicalHash(db ethdb.KeyValueReader, number uint32) common.Hash {
	data, _ := db.Get(rootCanonicalKey(number))
	return common.BytesToHash(data)
}

// WriteRootCanonicalHash stores the hash assigned to a canonical root height.
func WriteRootCanonicalHash(db ethdb.KeyValueWriter, hash common.Hash, number uint32) error {
	return db.Put(rootCanonicalKey(number), hash.Bytes())
}

type rootShardConfirmation struct {
	ChainID *big.Int
	Hash    common.Hash
}

// ReadRootConfirmedShardBlocks retrieves the cumulative shard confirmations for
// a root, including confirmations inherited from its ancestors.
// Missing snapshots, read failures and decoding failures return an error.
func ReadRootConfirmedShardBlocks(db ethdb.KeyValueReader, hash common.Hash) (map[string]common.Hash, error) {
	data, err := db.Get(rootConfirmedKey(hash))
	if err != nil {
		return nil, err
	}
	var entries []rootShardConfirmation
	if err := rlp.DecodeBytes(data, &entries); err != nil {
		return nil, fmt.Errorf("decode root shard confirmations: %w", err)
	}
	confirmed := make(map[string]common.Hash, len(entries))
	for _, entry := range entries {
		confirmed[entry.ChainID.String()] = entry.Hash
	}
	return confirmed, nil
}

// WriteRootConfirmedShardBlocks stores a cumulative snapshot for a root.
func WriteRootConfirmedShardBlocks(db ethdb.KeyValueWriter, hash common.Hash, confirmed map[string]common.Hash) error {
	entries := make([]rootShardConfirmation, 0, len(confirmed))
	for id, blockHash := range confirmed {
		chainID, ok := new(big.Int).SetString(id, 10)
		if !ok || chainID.String() != id {
			return fmt.Errorf("invalid root shard confirmation chain ID %q", id)
		}
		entries = append(entries, rootShardConfirmation{ChainID: chainID, Hash: blockHash})
	}
	// Map iteration is unordered; persist entries in numeric chain ID order.
	slices.SortFunc(entries, func(a, b rootShardConfirmation) int {
		return a.ChainID.Cmp(b.ChainID)
	})
	data, err := rlp.EncodeToBytes(entries)
	if err != nil {
		return fmt.Errorf("encode root shard confirmations: %w", err)
	}
	return db.Put(rootConfirmedKey(hash), data)
}

// ReadLastConfirmedBlockHash retrieves the hash of the latest shard block
// confirmed by the root chain up to the given root block. It is the zero hash if
// the root chain confirms no block of the shard yet.
func ReadLastConfirmedBlockHash(db ethdb.KeyValueReader, rootHash common.Hash) common.Hash {
	data, _ := db.Get(lastConfirmedKey(rootHash))
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// WriteLastConfirmedBlockHash stores the hash of the latest shard block
// confirmed by the root chain up to the given root block.
func WriteLastConfirmedBlockHash(db ethdb.KeyValueWriter, rootHash common.Hash, hash common.Hash) {
	if err := db.Put(lastConfirmedKey(rootHash), hash.Bytes()); err != nil {
		log.Crit("Failed to store last confirmed shard block hash", "err", err)
	}
}

// HasXShardDeposits reports whether the deposits sent to this shard by the block
// of another shard with the given hash are stored.
func HasXShardDeposits(db ethdb.KeyValueReader, hash common.Hash) bool {
	has, _ := db.Has(xShardDepositsKey(hash))
	return has
}

// ReadXShardDeposits retrieves the deposits sent to this shard by the block of
// another shard with the given hash. It returns nil if they are not stored, and
// an empty list if the block sent this shard nothing.
func ReadXShardDeposits(db ethdb.KeyValueReader, hash common.Hash) []*coretypes.DepositTx {
	data, _ := db.Get(xShardDepositsKey(hash))
	if len(data) == 0 {
		return nil
	}
	var deposits []*coretypes.DepositTx
	if err := rlp.DecodeBytes(data, &deposits); err != nil {
		log.Error("Invalid cross-shard deposits RLP", "hash", hash, "err", err)
		return nil
	}
	return deposits
}

// WriteXShardDeposits stores the deposits sent to this shard by the block of
// another shard with the given hash, as the RLP encoding of the list.
func WriteXShardDeposits(db ethdb.KeyValueWriter, hash common.Hash, deposits []*coretypes.DepositTx) {
	if err := WriteXShardDepositsWithError(db, hash, deposits); err != nil {
		log.Crit("Failed to store cross-shard deposits", "err", err)
	}
}

// WriteXShardDepositsWithError stores the same list as WriteXShardDeposits,
// returning encoding and write errors to callers that can retry delivery.
func WriteXShardDepositsWithError(db ethdb.KeyValueWriter, hash common.Hash, deposits []*coretypes.DepositTx) error {
	data, err := rlp.EncodeToBytes(deposits)
	if err != nil {
		return err
	}
	return db.Put(xShardDepositsKey(hash), data)
}

// IsShardBlockCommitted reports whether all cross-shard lists and the header
// have been acknowledged. Read errors must not be mistaken for a missing marker.
func IsShardBlockCommitted(db ethdb.KeyValueReader, hash common.Hash) (bool, error) {
	return db.Has(shardBlockCommitKey(hash))
}

// WriteShardBlockCommit records completed delivery, as pyquarkchain's commit_ key.
// Call only after master acknowledges the header; a failed write permits retry.
func WriteShardBlockCommit(db ethdb.KeyValueWriter, hash common.Hash) error {
	return db.Put(shardBlockCommitKey(hash), []byte{1})
}
