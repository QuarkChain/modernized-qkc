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

package eth

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

type xShardTestSource struct {
	headers   []*types.Header
	finalized uint64
	logs      []types.Log
	queries   int
	filterErr bool
}

func (s *xShardTestSource) GetBlockByNumber(number rpc.BlockNumber, full bool) *types.Header {
	if number == rpc.FinalizedBlockNumber {
		return s.headers[s.finalized]
	}
	return s.headers[number]
}

func (s *xShardTestSource) GetLogs(query struct {
	BlockHash *common.Hash
	Addresses []common.Address `json:"address"`
	Topics    [][]common.Hash
}) ([]types.Log, error) {
	s.queries++
	if query.BlockHash == nil || !reflect.DeepEqual(query.Addresses, []common.Address{params.XShardOutboxAddress}) ||
		!reflect.DeepEqual(query.Topics, [][]common.Hash{{vm.XShardSentMessageTopic}, {common.HexToHash("0x1")}}) {
		return nil, errors.New("incorrect outbox filter")
	}
	if *query.BlockHash == s.headers[1].Hash() {
		if s.filterErr {
			return nil, errors.New("source unavailable")
		}
		return s.logs, nil
	}
	return []types.Log{}, nil
}

type xShardFailDB struct{ ethdb.Database }
type xShardFailBatch struct{ ethdb.Batch }

func (db xShardFailDB) NewBatch() ethdb.Batch { return xShardFailBatch{db.Database.NewBatch()} }
func (b xShardFailBatch) Write() error        { return errors.New("database write failed") }

func TestXShardReceive(t *testing.T) {
	ctx := context.Background()
	source := &xShardTestSource{finalized: 1}
	for i := range 3 {
		header := &types.Header{Number: big.NewInt(int64(i)), Difficulty: new(big.Int), GasLimit: 30_000_000}
		if i > 0 {
			header.ParentHash = source.headers[i-1].Hash()
		}
		source.headers = append(source.headers, header)
	}
	want := types.Log{
		Address:     params.XShardOutboxAddress,
		Topics:      []common.Hash{vm.XShardSentMessageTopic, common.HexToHash("0x1"), common.HexToHash("0x1234"), common.HexToHash("0x5678")},
		Data:        []byte{0xca, 0xfe},
		BlockHash:   source.headers[1].Hash(),
		BlockNumber: 1,
		TxHash:      common.HexToHash("0xabcdef"),
		TxIndex:     2,
		Index:       3,
	}
	source.logs = append(source.logs, want)
	for _, mutate := range []func(*types.Log){
		func(l *types.Log) { l.Address = common.Address{} },
		func(l *types.Log) { l.Topics[0] = common.Hash{} },
		func(l *types.Log) { l.Topics[1] = common.HexToHash("0x2") },
		func(l *types.Log) { l.Topics = l.Topics[:2] },
		func(l *types.Log) { l.Removed = true },
	} {
		other := want
		other.Topics = append([]common.Hash(nil), want.Topics...)
		mutate(&other)
		source.logs = append(source.logs, other)
	}
	server := rpc.NewServer()
	defer server.Stop()
	if err := server.RegisterName("eth", source); err != nil {
		t.Fatal(err)
	}
	client := ethclient.NewClient(rpc.DialInProc(server))
	defer client.Close()
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	api := &QuarkChainAPI{db: db}
	checkCursor := func(number uint64) {
		t.Helper()
		n, hash, err := rawdb.ReadXShardCursor(db, 0)
		if err != nil || n != number || hash != source.headers[number].Hash() {
			t.Fatalf("cursor (%d, %s, %v), want block %d", n, hash, err, number)
		}
	}
	// RPC failure must not mark the unread source block as received.
	source.filterErr = true
	if err := syncXShardLogs(ctx, client, db, 0, 1); err == nil || err.Error() != "source unavailable" {
		t.Fatalf("expected source error, got %v", err)
	}
	checkCursor(0)
	if logs, err := api.GetXShardMessages(0, want.BlockHash); err != nil || logs != nil {
		t.Fatalf("unread block: got %v, %v", logs, err)
	}
	source.filterErr = false
	source.logs[0].BlockHash = source.headers[2].Hash()
	if err := syncXShardLogs(ctx, client, db, 0, 1); err == nil {
		t.Fatal("accepted log from a different block")
	}
	checkCursor(0)
	source.logs[0].BlockHash = want.BlockHash
	if err := syncXShardLogs(ctx, client, xShardFailDB{db}, 0, 1); err == nil || err.Error() != "database write failed" {
		t.Fatalf("expected database error, got %v", err)
	}
	checkCursor(0)
	if logs, _ := api.GetXShardMessages(0, want.BlockHash); logs != nil {
		t.Fatal("failed batch stored messages")
	}
	if err := syncXShardLogs(ctx, client, db, 0, 1); err != nil {
		t.Fatal(err)
	}
	checkCursor(1)
	if logs, err := api.GetXShardMessages(0, want.BlockHash); err != nil || !reflect.DeepEqual(logs, []types.Log{want}) {
		t.Fatalf("stored logs: got %v, %v", logs, err)
	}
	if logs, err := api.GetXShardMessages(2, want.BlockHash); err != nil || logs != nil {
		t.Fatalf("different source shard: got %v, %v", logs, err)
	}
	if logs, err := api.GetXShardMessages(0, source.headers[0].Hash()); err != nil || logs == nil || len(logs) != 0 {
		t.Fatalf("empty block: got %v, %v", logs, err)
	}
	if logs, _ := api.GetXShardMessages(0, source.headers[2].Hash()); logs != nil {
		t.Fatal("stored unfinalized block")
	}
	queries := source.queries
	if err := syncXShardLogs(ctx, client, db, 0, 1); err != nil || source.queries != queries {
		t.Fatalf("restart reread stored logs: %v", err)
	}
	source.finalized = 2
	if err := syncXShardLogs(ctx, client, db, 0, 1); err != nil {
		t.Fatal(err)
	}
	checkCursor(2)
	source.headers[2].Extra = []byte{1}
	if err := syncXShardLogs(ctx, client, db, 0, 1); err == nil {
		t.Fatal("accepted changed source chain")
	}
	source.headers[2].Extra = nil
	checkCursor(2)
}
