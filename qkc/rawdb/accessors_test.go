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
	"bytes"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	coretypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// Full reads retain shard headers and tracking data; header reads skip the body.
func TestRootBlockStorage(t *testing.T) {
	db := rawdb.NewMemoryDatabase()

	header := &types.RootBlockHeader{
		Number:          7,
		ParentHash:      common.Hash{0x01},
		Coinbase:        common.Address{0x02},
		CoinbaseAmount:  *uint256.NewInt(3),
		Time:            1000,
		Difficulty:      *uint256.NewInt(4),
		TotalDifficulty: *uint256.NewInt(5),
		Nonce:           6,
		Extra:           []byte("root"),
	}
	shardHeaders := types.ShardBlockHeaders{
		{Number: big.NewInt(1), Difficulty: common.Big0, Extra: []byte("shard")},
	}
	block := types.NewRootBlock(header, shardHeaders, []byte("tracking"))
	hash := block.Hash()

	if HasRootBlock(db, hash) {
		t.Fatal("non existent root block returned")
	}
	if entry := ReadRootBlock(db, hash); entry != nil {
		t.Fatalf("non existent root block returned: %v", entry)
	}
	if entry := ReadRootBlockHeader(db, hash); entry != nil {
		t.Fatalf("non existent root block header returned: %v", entry)
	}
	WriteRootBlock(db, block)

	if !HasRootBlock(db, hash) {
		t.Fatal("stored root block not found")
	}
	entry := ReadRootBlock(db, hash)
	if entry == nil {
		t.Fatal("stored root block not found")
	}
	if entry.Hash() != hash {
		t.Fatalf("retrieved root block mismatch: have %x, want %x", entry.Hash(), hash)
	}
	if len(entry.ShardBlockHeaders()) != 1 || entry.ShardBlockHeaders()[0].Hash() != shardHeaders[0].Hash() {
		t.Fatal("retrieved shard block headers mismatch")
	}
	if string(entry.TrackingData()) != "tracking" {
		t.Fatalf("retrieved tracking data mismatch: have %q", entry.TrackingData())
	}
	headerEntry := ReadRootBlockHeader(db, hash)
	if headerEntry == nil {
		t.Fatal("stored root block header not found")
	}
	if headerEntry.Hash() != hash {
		t.Fatalf("retrieved root block header mismatch: have %x, want %x", headerEntry.Hash(), hash)
	}
}

// Persisted roots include their canonical index and cumulative confirmations.
// The snapshot retains full chain IDs and must be independent per read.
// This covers RootDb.put_root_block's block/lastlist_ split using the local format.
func TestRootChainStorage(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	block := types.NewRootBlock(&types.RootBlockHeader{
		Number: 1, Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(2),
	}, nil, nil)
	hash := block.Hash()
	confirmed := map[string]common.Hash{
		"10001": {1}, new(big.Int).Lsh(big.NewInt(1), 128).String(): {2},
	}
	batch := db.NewBatch()
	defer batch.Close()
	WriteRootBlock(batch, block)
	if err := WriteRootConfirmedShardBlocks(batch, hash, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := WriteRootCanonicalHash(batch, hash, 1); err != nil {
		t.Fatal(err)
	}
	if err := WriteRootHeadBlockHash(batch, hash); err != nil {
		t.Fatal(err)
	}
	if err := WriteRootCommittingHash(batch, hash); err != nil {
		t.Fatal(err)
	}
	if stored := ReadRootBlock(db, hash); stored != nil {
		t.Fatalf("uncommitted block visible: %v", stored)
	}
	if err := batch.Write(); err != nil {
		t.Fatal(err)
	}
	if stored := ReadRootBlock(db, hash); stored == nil || stored.Hash() != hash {
		t.Fatalf("block round trip: %v", stored)
	}
	if header := ReadRootBlockHeader(db, hash); header == nil || header.Hash() != hash {
		t.Fatalf("header round trip: %v", header)
	}
	for _, read := range []func() (common.Hash, error){
		func() (common.Hash, error) { return ReadRootHeadBlockHash(db) },
		func() (common.Hash, error) { return ReadRootCommittingHash(db) },
	} {
		if got, err := read(); err != nil || got != hash {
			t.Fatalf("hash round trip: have %x, want %x, error %v", got, hash, err)
		}
	}
	if got := ReadRootCanonicalHash(db, 1); got != hash {
		t.Fatalf("canonical hash round trip: have %x, want %x", got, hash)
	}
	got, err := ReadRootConfirmedShardBlocks(db, hash)
	if err != nil || !reflect.DeepEqual(got, confirmed) {
		t.Fatalf("confirmation round trip: %v, error %v", got, err)
	}
	delete(got, "10001")
	if again, err := ReadRootConfirmedShardBlocks(db, hash); err != nil || !reflect.DeepEqual(again, confirmed) {
		t.Fatal("confirmation reads failed or share mutable data", err)
	}
	if err := DeleteRootCommittingHash(db); err != nil {
		t.Fatal(err)
	}
	if hash, err := ReadRootCommittingHash(db); err != nil || hash != (common.Hash{}) {
		t.Fatal("pending delivery was not cleared", err)
	}
}

type rootReadFailure struct {
	ethdb.Database
	err error
}

func (db rootReadFailure) Has([]byte) (bool, error) {
	panic("read accessors must not call Has")
}

func (db rootReadFailure) Get([]byte) ([]byte, error) { return nil, db.err }

// The other read accessors retain geth's empty-value behavior on read failures.
func TestRootChainStorageReadFailure(t *testing.T) {
	db := rootReadFailure{Database: rawdb.NewMemoryDatabase(), err: errors.New("database failed")}
	t.Cleanup(func() { db.Close() })
	hash := common.Hash{1}
	if ReadRootCanonicalHash(db, 1) != (common.Hash{}) {
		t.Fatal("read failure returned a hash")
	}
	if ReadRootBlock(db, hash) != nil || ReadRootBlockHeader(db, hash) != nil {
		t.Fatal("read failure returned root data")
	}
	if got, err := ReadRootConfirmedShardBlocks(db, hash); got != nil || !errors.Is(err, db.err) {
		t.Fatal("confirmation read error cause lost", got, err)
	}
}

type recoveryHashReader struct {
	exists bool
	hasErr error
	data   []byte
	getErr error
}

func (db recoveryHashReader) Has([]byte) (bool, error) { return db.exists, db.hasErr }
func (db recoveryHashReader) Get([]byte) ([]byte, error) {
	if !db.exists || db.hasErr != nil {
		panic("Get called after absence or Has failure")
	}
	return db.data, db.getErr
}

func TestRootHeadBlockHashReadErrors(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	if hash, err := ReadRootHeadBlockHash(db); hash != (common.Hash{}) || err == nil {
		t.Fatalf("missing head: hash %x, error %v", hash, err)
	}
	failure := errors.New("head read failed")
	reader := rootReadFailure{Database: db, err: failure}
	if hash, err := ReadRootHeadBlockHash(reader); hash != (common.Hash{}) || !errors.Is(err, failure) {
		t.Fatalf("head read error lost: hash %x, error %v", hash, err)
	}
}

func TestRootCommittingHashReads(t *testing.T) {
	failure := errors.New("database read failed")
	hash := common.Hash{1}
	for _, test := range []struct {
		name    string
		db      recoveryHashReader
		want    common.Hash
		wantErr bool
	}{
		{"absent", recoveryHashReader{}, common.Hash{}, false},
		{"stored", recoveryHashReader{exists: true, data: hash.Bytes()}, hash, false},
		{"has failure", recoveryHashReader{hasErr: failure}, common.Hash{}, true},
		{"get failure", recoveryHashReader{exists: true, getErr: failure}, common.Hash{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ReadRootCommittingHash(test.db)
			if got != test.want || (err != nil) != test.wantErr {
				t.Fatalf("have %x, %v; want %x, error %v", got, err, test.want, test.wantErr)
			}
			if (test.db.hasErr != nil || test.db.getErr != nil) && !errors.Is(err, failure) {
				t.Fatal("read error cause lost", err)
			}
		})
	}
}

func TestRootConfirmedShardBlocksEncoding(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	hash := common.Hash{1}
	if got, err := ReadRootConfirmedShardBlocks(db, hash); got != nil || err == nil {
		t.Fatal("missing snapshot returned confirmations or no error", got, err)
	}
	if err := WriteRootConfirmedShardBlocks(db, hash, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRootConfirmedShardBlocks(db, hash); err != nil || got == nil || len(got) != 0 {
		t.Fatal("empty snapshot treated as missing or failed to decode", err)
	}
	largeID := new(big.Int).Lsh(big.NewInt(1), 128)
	confirmed := map[string]common.Hash{"10": {1}, "2": {2}, largeID.String(): {3}}
	want, err := rlp.EncodeToBytes([]rootShardConfirmation{
		{big.NewInt(2), common.Hash{2}},
		{big.NewInt(10), common.Hash{1}},
		{largeID, common.Hash{3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := WriteRootConfirmedShardBlocks(db, hash, confirmed); err != nil {
			t.Fatal(err)
		}
		data, err := db.Get(rootConfirmedKey(hash))
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("noncanonical snapshot encoding: %x, %v", data, err)
		}
	}
}

func TestRootStorageInvalidEncoding(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	hash := common.Hash{1}
	if err := db.Put(rootBlockKey(hash), []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	if ReadRootBlock(db, hash) != nil || ReadRootBlockHeader(db, hash) != nil {
		t.Fatal("invalid SSZ returned root data")
	}
	for _, data := range [][]byte{{}, {0xff}} {
		if err := db.Put(rootConfirmedKey(hash), data); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadRootConfirmedShardBlocks(db, hash); got != nil || err == nil {
			t.Fatal("invalid RLP returned confirmations or no error", got, err)
		}
	}
}

func TestLastConfirmedStorage(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	root := common.Hash{0x01}

	if hash := ReadLastConfirmedBlockHash(db, root); hash != (common.Hash{}) {
		t.Fatalf("non existent last confirmed block returned: %x", hash)
	}
	want := common.Hash{0x02}
	WriteLastConfirmedBlockHash(db, root, want)
	if hash := ReadLastConfirmedBlockHash(db, root); hash != want {
		t.Fatalf("last confirmed block mismatch: have %x, want %x", hash, want)
	}
	if hash := ReadLastConfirmedBlockHash(db, common.Hash{0x03}); hash != (common.Hash{}) {
		t.Fatalf("last confirmed block of another root returned: %x", hash)
	}
}

func TestXShardDepositsStorage(t *testing.T) {
	var (
		db    = rawdb.NewMemoryDatabase()
		empty = common.Hash{0x01} // a block sending this shard nothing
		full  = common.Hash{0x02}
	)
	deposits := []*coretypes.DepositTx{
		{
			SourceBlockHash: full,
			SourceLogIndex:  0,
			From:            common.Address{0x0a},
			To:              &common.Address{0x0b},
			Value:           uint256.NewInt(1),
			GasPrice:        uint256.NewInt(2),
			Gas:             30000,
			Data:            []byte{0xca, 0xfe},
		},
		{
			SourceBlockHash: full,
			SourceLogIndex:  3,
			From:            common.Address{0x0c},
			To:              &common.Address{0x0d},
			Value:           uint256.NewInt(4),
			GasPrice:        uint256.NewInt(5),
			Gas:             40000,
			Data:            []byte{0xbe, 0xef},
		},
	}
	for _, hash := range []common.Hash{empty, full} {
		if HasXShardDeposits(db, hash) || ReadXShardDeposits(db, hash) != nil {
			t.Fatalf("block %x: deposits stored before writing", hash)
		}
	}
	WriteXShardDeposits(db, empty, nil)
	WriteXShardDeposits(db, full, deposits)

	// An empty list is stored, and read back as an empty list, not as missing.
	if !HasXShardDeposits(db, empty) {
		t.Error("empty deposit list not stored")
	}
	if have := ReadXShardDeposits(db, empty); have == nil || len(have) != 0 {
		t.Errorf("empty deposit list read as %v", have)
	}
	if !HasXShardDeposits(db, full) {
		t.Error("deposit list not stored")
	}
	if have := ReadXShardDeposits(db, full); !reflect.DeepEqual(have, deposits) {
		t.Errorf("read deposits %+v, want %+v", have, deposits)
	}
	// The value is the RLP encoding of the list, keyed by the source block hash.
	want, _ := rlp.EncodeToBytes(deposits)
	if have, _ := db.Get(append([]byte("qkc_xShard"), full.Bytes()...)); !bytes.Equal(have, want) {
		t.Errorf("stored %x, want %x", have, want)
	}
}

// Storage accessors preserve the cause so callers can decide whether to retry.
type failingWriter struct{ err error }

func (db failingWriter) Put([]byte, []byte) error { return db.err }
func (db failingWriter) Delete([]byte) error      { return db.err }

func TestRootChainWriteErrors(t *testing.T) {
	failure := errors.New("database write failed")
	db := failingWriter{failure}
	hash := common.Hash{1}
	for _, test := range []struct {
		name  string
		write func() error
	}{
		{"root head", func() error { return WriteRootHeadBlockHash(db, hash) }},
		{"committing root", func() error { return WriteRootCommittingHash(db, hash) }},
		{"clear committing root", func() error { return DeleteRootCommittingHash(db) }},
		{"canonical root", func() error { return WriteRootCanonicalHash(db, hash, 1) }},
		{"root confirmations", func() error { return WriteRootConfirmedShardBlocks(db, hash, map[string]common.Hash{"10001": hash}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.write(); !errors.Is(err, failure) {
				t.Fatalf("write error lost: %v", err)
			}
		})
	}
}

func TestRootConfirmationEncodingErrors(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	hash := common.Hash{1}
	confirmed := map[string]common.Hash{"10001": hash}
	if err := WriteRootConfirmedShardBlocks(db, hash, confirmed); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"invalid", "01", "-1"} {
		t.Run(id, func(t *testing.T) {
			if err := WriteRootConfirmedShardBlocks(db, hash, map[string]common.Hash{id: hash}); err == nil {
				t.Fatal("invalid chain ID accepted")
			}
			if got, err := ReadRootConfirmedShardBlocks(db, hash); err != nil || !reflect.DeepEqual(got, confirmed) {
				t.Fatal("encoding failure overwrote the stored snapshot or read failed", err)
			}
		})
	}
}
