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
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

const blockCacheLimit = 4096 // Match pyquarkchain's RootDb block cache.

var (
	ErrRootDataMissing = errors.New("missing root chain data")
	ErrNotImplemented  = errors.New("root chain: function not implemented")
	errChainStopped    = errors.New("root chain is stopped")
)

// RootChain manages root-chain data and recovery using a caller-provided database,
// client and consensus engine. The backend owns their lifetimes and cancels
// backendCtx before calling Close. Configuration must match the engine and stay
// unchanged after New. The caller must drain in-flight operations before closing
// shared resources.
// TODO(backend): Call Init after cluster connections are ready and before
// starting sync, mining or imports.
type RootChain struct {
	backendCtx context.Context
	config     *params.QuarkChainConfig
	client     ClusterClient
	db         ethdb.Database
	engine     posw.RootEngine

	// Blocks are immutable and keyed by hash, so side branches share this cache.
	blockCache *lru.Cache[common.Hash, *types.RootBlock]

	// Serialize chain writes, including Init. Readers use atomic state or the DB.
	chainmu   *syncx.ClosableMutex
	closeOnce sync.Once

	// Published headers are immutable; updates replace the tip pointer.
	tip   atomic.Pointer[types.RootBlockHeader]
	ready atomic.Bool
}

var _ posw.RootChainReader = (*RootChain)(nil)

// New recovers the persisted root head as the runtime tip, as in goquarkchain,
// without rewinding or changing the stored head.
// It binds the supplied client and engine without taking ownership of them or
// making cluster RPCs. The caller must validate configuration when creating the engine.
// backendCtx controls RootChain's lifetime and is canceled only by the backend.
// Call SetupRootGenesisBlock before New to initialize or verify the local database.
// TODO: Root insertion must persist the block, its confirmations and canonical
// head updates atomically so recovery never observes a partially stored tip.
func New(backendCtx context.Context, config *params.QuarkChainConfig, client ClusterClient, db ethdb.Database, engine posw.RootEngine) (*RootChain, error) {
	if backendCtx == nil || config == nil || client == nil || db == nil || engine == nil {
		return nil, errors.New("missing backend context, root configuration, cluster client, database or engine")
	}
	if err := backendCtx.Err(); err != nil {
		return nil, err
	}
	tip, err := readRootHead(db, config)
	if err != nil {
		return nil, err
	}
	c := &RootChain{
		backendCtx: backendCtx,
		config:     config, client: client, db: db, engine: engine,
		chainmu:    syncx.NewClosableMutex(),
		blockCache: lru.NewCache[common.Hash, *types.RootBlock](blockCacheLimit),
	}
	c.blockCache.Add(tip.Hash(), tip)
	c.tip.Store(tip.Header())
	return c, nil
}

func readRootHead(db ethdb.KeyValueReader, config *params.QuarkChainConfig) (*types.RootBlock, error) {
	hash, err := rawdb.ReadRootHeadBlockHash(db)
	if err != nil {
		return nil, fmt.Errorf("read persisted root head: %w", err)
	}
	if hash == (common.Hash{}) {
		return nil, fmt.Errorf("%w: persisted root head", ErrRootDataMissing)
	}
	tip := rawdb.ReadRootBlock(db, hash)
	if tip == nil {
		return nil, fmt.Errorf("%w: persisted root tip", ErrRootDataMissing)
	}
	if tip.Hash() != hash {
		return nil, fmt.Errorf("root block does not match stored hash %x", hash)
	}
	// Hash and height lookups must agree before publishing the persisted tip.
	if canonical := rawdb.ReadRootCanonicalHash(db, tip.Number()); canonical != hash {
		return nil, fmt.Errorf("canonical root hash at height %d does not match persisted head: have %x, want %x", tip.Number(), canonical, hash)
	}
	confirmed, err := rawdb.ReadRootConfirmedShardBlocks(db, hash)
	if err != nil {
		return nil, fmt.Errorf("read confirmations for root %x: %w", hash, err)
	}
	if err := checkConfirmations(config, tip, confirmed); err != nil {
		return nil, err
	}
	return tip, nil
}

// Recovery requires exactly one nonzero confirmation for every configured shard.
// Non-genesis tips must also match their body commitment and the last carried
// header for each shard; shards absent from the body retain inherited confirmations.
func checkConfirmations(config *params.QuarkChainConfig, tip *types.RootBlock, confirmed map[string]common.Hash) error {
	hash := tip.Hash()
	if len(confirmed) != len(config.ChainIDs) {
		return fmt.Errorf("root %x shard confirmation count mismatch: have %d, want %d", hash, len(confirmed), len(config.ChainIDs))
	}
	for _, id := range config.ChainIDs {
		if confirmed[id.String()] == (common.Hash{}) {
			return fmt.Errorf("%w: root %x chain %s confirmation", ErrRootDataMissing, hash, id)
		}
	}
	if tip.Number() == 0 {
		return nil
	}
	headers := tip.ShardBlockHeaders()
	if types.CalcShardHeaderHash(headers) != tip.ShardHeaderHash() {
		return fmt.Errorf("root %x shard header commitment mismatch", hash)
	}
	last := make(map[string]common.Hash)
	for _, header := range headers {
		extra, err := header.QKCExtra()
		if err != nil {
			return fmt.Errorf("decode shard header %x in root %x: %w", header.Hash(), hash, err)
		}
		last[extra.ChainID.String()] = header.Hash()
	}
	for id, want := range last {
		if got := confirmed[id]; got != want {
			return fmt.Errorf("root %x chain %s confirmation mismatch: have %x, want %x", hash, id, got, want)
		}
	}
	return nil
}

// Init sends the persisted root tip recovered by New to every shard in PING.
// Shards that already store this root block restore their confirmed shard head
// from it. Otherwise, they must restore its locally stored direct parent, then
// import the supplied root block through the normal root-block path. Missing
// parents or required shard blocks, state or cross-shard data must return an error.
// TODO(shard): Implement recovery when the supplied root block is absent locally;
// ShardChain.Init currently requires non-genesis roots and their confirmations
// to already be stored locally.
// Connections must be ready before this call. Init waits for all shards to
// succeed before publishing Ready and uses backendCtx for PING cancellation.
// On failure, the backend must abort startup and stop the node.
// Do not retry Init: shards may already have handled PING. Recovery requires
// a cluster restart. Repeating a successful Init is a no-op.
// Init does not change the local tip.
func (c *RootChain) Init() error {
	if !c.chainmu.TryLock() {
		return errChainStopped
	}
	defer c.chainmu.Unlock()
	if err := c.backendCtx.Err(); err != nil {
		return err
	}
	if c.ready.Load() {
		return nil
	}
	block, err := c.GetRootBlock(c.tip.Load().Hash())
	if err != nil {
		return err
	}
	if block == nil {
		return fmt.Errorf("%w: recovery checkpoint", ErrRootDataMissing)
	}
	// Every shard must acknowledge initialization at the master's persisted tip.
	if err := c.client.Ping(c.backendCtx, block); err != nil {
		return fmt.Errorf("initialize shards: %w", err)
	}
	c.ready.Store(true)
	return nil
}

// Close waits for chain writes to finish and prevents further writes. It is safe
// to call repeatedly or concurrently. Before calling Close, the backend must
// cancel backendCtx and unblock pending RPCs, closing connections if needed.
// Readers must be drained before shared resources are closed. Context cancellation
// belongs to the backend.
func (c *RootChain) Close() {
	c.closeOnce.Do(func() {
		c.chainmu.Close()
		c.ready.Store(false)
	})
}

// Ready reports whether Init completed, the backend context is active and the
// chain has not been closed.
// Sync, mining and imports must wait for it.
func (c *RootChain) Ready() bool { return c.ready.Load() && c.backendCtx.Err() == nil }

// RootTip returns the recovered local tip, including before Init.
// The returned header is read-only; callers must copy it before modification.
func (c *RootChain) RootTip() *types.RootBlockHeader {
	return c.tip.Load()
}

// GetRootHeader reads a header by hash from any stored branch.
// It shares GetRootBlock's cache and unavailable-data convention, returning a
// copy of the header. Consensus treats an unavailable header as unknown ancestry.
func (c *RootChain) GetRootHeader(hash common.Hash) (*types.RootBlockHeader, error) {
	block, err := c.GetRootBlock(hash)
	if err != nil || block == nil {
		return nil, err
	}
	return block.Header(), nil
}

// GetRootBlock reads a block by hash from any stored branch.
// Like geth, missing or unreadable data and SSZ decode failures return nil
// without an error; decoded blocks with the wrong hash return an error.
// Startup stops if its persisted tip cannot be loaded.
// Returned blocks are shared through the cache and must not be modified.
func (c *RootChain) GetRootBlock(hash common.Hash) (*types.RootBlock, error) {
	if block, ok := c.blockCache.Get(hash); ok {
		return block, nil
	}
	block := rawdb.ReadRootBlock(c.db, hash)
	if block == nil {
		return nil, nil
	}
	if block.Hash() != hash {
		return nil, fmt.Errorf("root block does not match stored hash %x", hash)
	}
	c.blockCache.Add(hash, block)
	return block, nil
}

// GetRootHeaderByNumber reads the stored canonical header.
// A missing or unreadable index returns nil without an error, as in geth.
func (c *RootChain) GetRootHeaderByNumber(number uint32) (*types.RootBlockHeader, error) {
	hash := rawdb.ReadRootCanonicalHash(c.db, number)
	if hash == (common.Hash{}) {
		return nil, nil
	}
	return c.GetRootHeader(hash)
}

// GetRootBlockByNumber follows the stored canonical index.
// A missing or unreadable index returns nil without an error, as in geth.
func (c *RootChain) GetRootBlockByNumber(number uint32) (*types.RootBlock, error) {
	hash := rawdb.ReadRootCanonicalHash(c.db, number)
	if hash == (common.Hash{}) {
		return nil, nil
	}
	return c.GetRootBlock(hash)
}

// GetLastConfirmedShardBlocks reads this root's cumulative branch snapshot.
func (c *RootChain) GetLastConfirmedShardBlocks(hash common.Hash) (map[string]common.Hash, error) {
	confirmed, err := rawdb.ReadRootConfirmedShardBlocks(c.db, hash)
	if err != nil {
		return nil, fmt.Errorf("read confirmations for root %x: %w", hash, err)
	}
	return confirmed, nil
}

// GetShardBlockCoinbaseAmount returns validated shard income.
// TODO: Connect shard reports to the income store.
func (c *RootChain) GetShardBlockCoinbaseAmount(common.Hash) (*uint256.Int, error) {
	return nil, ErrNotImplemented
}

// GetRootChainStakes returns stakes and signer at the confirmed shard state.
// TODO: Connect the client query with the current import's context.
func (c *RootChain) GetRootChainStakes(*big.Int, common.Hash, common.Address) (*uint256.Int, common.Address, error) {
	return nil, common.Address{}, ErrNotImplemented
}
