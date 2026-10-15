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
	"fmt"
	"math/big"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// QuarkChainAPI exposes received messages without executing them.
type QuarkChainAPI struct {
	db ethdb.Database
}

// GetXShardMessages reads locally stored messages, not logs from the source RPC.
func (api *QuarkChainAPI) GetXShardMessages(sourceShard uint32, blockHash common.Hash) ([]types.Log, error) {
	return rawdb.ReadXShardMessages(api.db, sourceShard, blockHash)
}

func (s *Ethereum) receiveXShardLogs(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		client, err := ethclient.DialContext(pollCtx, s.config.XShardSource.RPC)
		if err == nil {
			err = syncXShardLogs(pollCtx, client, s.chainDb, s.config.XShardSource.ShardID, s.blockchain.Config().QuarkChain.ShardID)
			client.Close()
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Warn("Failed to read cross-shard Outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// syncXShardLogs reads finalized blocks by hash. Messages and progress are
// committed together so retries and restarts cannot lose or duplicate a block.
//
// TODO: When destination execution is implemented, resume consumption from the
// parent block's QKCExtra.Cursor and validate the resulting cursor in each
// destination block. The download cursor below tracks ingestion only.
func syncXShardLogs(ctx context.Context, client *ethclient.Client, db ethdb.Database, source, destination uint32) error {
	number, hash, err := rawdb.ReadXShardDownloadCursor(db, source)
	if err != nil {
		return err
	}
	finalized, err := client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return err
	}
	if hash != (common.Hash{}) {
		header, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
		if err != nil {
			return err
		}
		if header.Hash() != hash || finalized.Number.Uint64() < number {
			return fmt.Errorf("xshard source changed at stored block %d", number)
		}
		number++
	}
	dstTopic := common.BigToHash(new(big.Int).SetUint64(uint64(destination)))
	for ; number <= finalized.Number.Uint64(); number++ {
		header, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
		if err != nil {
			return err
		}
		if hash != (common.Hash{}) && header.ParentHash != hash {
			return fmt.Errorf("xshard source ancestry changed at block %d", number)
		}
		blockHash := header.Hash()
		logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
			BlockHash: &blockHash,
			Addresses: []common.Address{params.XShardOutboxAddress},
			Topics:    [][]common.Hash{{vm.XShardSentMessageTopic}, {dstTopic}},
		})
		if err != nil {
			return err
		}
		messages := make([]types.Log, 0, len(logs))
		for _, event := range logs {
			if event.Removed || event.Address != params.XShardOutboxAddress || len(event.Topics) != 4 || event.Topics[0] != vm.XShardSentMessageTopic || event.Topics[1] != dstTopic {
				continue
			}
			if event.BlockHash != blockHash || event.BlockNumber != number {
				return fmt.Errorf("xshard log does not belong to source block %d", number)
			}
			messages = append(messages, event)
		}
		batch := db.NewBatch()
		if err := rawdb.WriteXShardMessages(batch, source, blockHash, messages); err != nil {
			return err
		}
		if err := rawdb.WriteXShardDownloadCursor(batch, source, number, blockHash); err != nil {
			return err
		}
		if err := batch.Write(); err != nil {
			return err
		}
		hash = blockHash
	}
	return nil
}
