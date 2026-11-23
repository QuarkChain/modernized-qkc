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

// Match pyquarkchain's MAX_STALE_ROOT_BLOCK_HEIGHT_DIFF admission limit.
const maxStaleRootBlockHeightDiff uint32 = 22500

var (
	ErrRootDataMissing     = errors.New("missing root chain data")
	ErrNotImplemented      = errors.New("root chain: function not implemented")
	errChainNotInitialized = errors.New("root chain is not initialized")
	errChainStopped        = errors.New("root chain is stopped")
	errChainNotReady       = errors.New("root chain is not ready")
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
	builder    *RootBlockBuilder

	// Blocks are immutable and keyed by hash, so side branches share this cache.
	blockCache *lru.Cache[common.Hash, *types.RootBlock]

	// Serialize chain writes, including Init. Readers use atomic state or the DB.
	chainmu   *syncx.ClosableMutex
	closeOnce sync.Once

	// Published headers are immutable; updates replace the tip pointer.
	tip atomic.Pointer[types.RootBlockHeader]

	// Initialization stays recorded when delivery fails or Close begins.
	initialized atomic.Bool
	// Stopping is terminal and makes Ready false before in-flight writes finish.
	stopping atomic.Bool
}

var _ posw.RootChainReader = (*RootChain)(nil)

// New recovers the persisted root head as the runtime tip, as in goquarkchain,
// without rewinding or changing the stored head.
// It binds the supplied client and engine without taking ownership of them or
// making cluster RPCs. The caller must validate configuration when creating the engine.
// backendCtx controls RootChain's lifetime and is canceled only by the backend.
// Genesis, canonical indices and cumulative confirmations must already be stored.
func New(backendCtx context.Context, config *params.QuarkChainConfig, client ClusterClient, db ethdb.Database, engine posw.RootEngine) (*RootChain, error) {
	if backendCtx == nil || config == nil || client == nil || db == nil || engine == nil {
		return nil, errors.New("missing backend context, root configuration, cluster client, database or engine")
	}
	if err := backendCtx.Err(); err != nil {
		return nil, err
	}
	// TODO(backend): Add SetupRootGenesisBlock and call it before New to set up
	// genesis. For an empty database, atomically store genesis, its cumulative shard
	// confirmations, the canonical index and root head. For an initialized database,
	// validate genesis without replacing the persisted head.
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
	c := &RootChain{
		backendCtx: backendCtx,
		config:     config, client: client, db: db, engine: engine,
		chainmu:    syncx.NewClosableMutex(),
		blockCache: lru.NewCache[common.Hash, *types.RootBlock](blockCacheLimit),
	}
	if err := c.checkConfirmations(tip); err != nil {
		return nil, err
	}
	c.builder, err = NewRootBlockBuilder(config, c, engine, client)
	if err != nil {
		return nil, err
	}
	c.blockCache.Add(hash, tip)
	c.tip.Store(tip.Header())
	return c, nil
}

// Recovery requires exactly one nonzero confirmation for every configured shard.
// Non-genesis tips must also match their body commitment and the last carried
// header for each shard; shards absent from the body retain inherited confirmations.
func (c *RootChain) checkConfirmations(tip *types.RootBlock) error {
	hash := tip.Hash()
	confirmed, err := c.readConfirmations(hash)
	if err != nil {
		return err
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

// readConfirmations requires exactly one nonzero confirmation for every
// configured shard, including shards omitted by the next root's body.
func (c *RootChain) readConfirmations(hash common.Hash) (map[string]common.Hash, error) {
	confirmed, err := c.GetLastConfirmedShardBlocks(hash)
	if err != nil {
		return nil, err
	}
	if len(confirmed) != len(c.config.ChainIDs) {
		return nil, fmt.Errorf("root %x shard confirmation count mismatch: have %d, want %d", hash, len(confirmed), len(c.config.ChainIDs))
	}
	for _, id := range c.config.ChainIDs {
		if confirmed[id.String()] == (common.Hash{}) {
			return nil, fmt.Errorf("%w: root %x chain %s confirmation", ErrRootDataMissing, hash, id)
		}
	}
	return confirmed, nil
}

// Init sends the persisted root tip recovered by New to every shard in PING.
// Shards that already store this root block restore their confirmed shard head
// from it. Otherwise, they must restore its locally stored direct parent, then
// import the supplied root block through the normal root-block path. Missing
// parents or required shard blocks, state or cross-shard data must return an error.
// TODO(shard): Implement recovery when the supplied root block is absent locally;
// ShardChain.Init currently requires non-genesis roots and their confirmations
// to already be stored locally.
// Connections must be ready before this call. Init records initialization after
// all shards succeed, using backendCtx for PING cancellation. Ready becomes true
// only while backendCtx is active and the chain is not stopping.
// On failure, the backend must abort startup and stop the node.
// Do not retry Init: shards may already have handled PING. Recovery requires
// a cluster restart. Repeating a successful Init is a no-op until the chain stops;
// calls after that return errChainStopped.
// Init does not change the local tip.
func (c *RootChain) Init() error {
	if !c.chainmu.TryLock() {
		return errChainStopped
	}
	defer c.chainmu.Unlock()
	if c.stopping.Load() {
		return errChainStopped
	}
	if err := c.backendCtx.Err(); err != nil {
		return err
	}
	if c.initialized.Load() {
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
	c.initialized.Store(true)
	return nil
}

// InsertBlock verifies and stores a root block, then delivers it to all shards.
// Init must complete first. Insertion and delivery use backendCtx, so cancellation
// is controlled by the backend. Delivery failures leave the local commit intact,
// disable Init and insertion, and return an error. The client handles connection
// failures and requests master shutdown for cluster recovery.
func (c *RootChain) InsertBlock(block *types.RootBlock) error {
	if block == nil || block.Number() == 0 {
		return errors.New("missing or genesis root block")
	}
	if !c.chainmu.TryLock() {
		return errChainStopped
	}
	defer c.chainmu.Unlock()
	if c.stopping.Load() {
		return errChainStopped
	}
	if err := c.backendCtx.Err(); err != nil {
		return err
	}
	if !c.initialized.Load() {
		return errChainNotInitialized
	}
	// Reject stale blocks before loading, verifying or broadcasting them, even
	// if already stored. Compare heights first to avoid unsigned underflow.
	if tip := c.tip.Load().Number; tip > block.Number() && tip-block.Number() > maxStaleRootBlockHeightDiff {
		return fmt.Errorf("root block is too old: height %d, tip %d, maximum difference %d", block.Number(), tip, maxStaleRootBlockHeightDiff)
	}
	hash := block.Hash()
	stored, err := c.GetRootBlock(hash)
	batch := c.db.NewBatch()
	defer batch.Close()
	if err == nil && stored != nil {
		// The hash identifies only the header. Use the previously verified DB
		// body, not the supplied body, and require its atomic confirmation record.
		block = stored
		if _, err := c.readConfirmations(hash); err != nil {
			return err
		}
	} else {
		// Missing, unreadable or hash-mismatched records are repaired only after
		// fully verifying the supplied block and atomically rewriting its snapshot.
		// Reader methods do not acquire chainmu, so engine queries can run while
		// insertion holds the write lock without reentering it.
		if err := c.engine.VerifyBlock(c, block); err != nil {
			return fmt.Errorf("verify root block: %w", err)
		}
		confirmed, err := c.readConfirmations(block.ParentHash())
		if err != nil {
			return err
		}
		// Each snapshot inherits chains omitted by this root from its parent
		// branch, then advances chains to the last header confirmed in this root.
		for _, header := range block.ShardBlockHeaders() {
			extra, err := header.QKCExtra()
			if err != nil {
				return err
			}
			confirmed[extra.ChainID.String()] = header.Hash()
		}
		if err := rawdb.WriteRootBlockWithError(batch, block); err != nil {
			return fmt.Errorf("store root block: %w", err)
		}
		if err := rawdb.WriteRootConfirmedShardBlocks(batch, hash, confirmed); err != nil {
			return fmt.Errorf("store root confirmations: %w", err)
		}
	}
	header := block.Header()
	// Stored blocks can belong to side branches. Re-evaluate fork choice and
	// promote a block only when its TD strictly exceeds the current runtime tip.
	updateTip := header.TotalDifficulty.Cmp(&c.tip.Load().TotalDifficulty) > 0
	if updateTip {
		if err := c.rewriteCanonical(batch, header); err != nil {
			return err
		}
		if err := rawdb.WriteRootHeadBlockHash(batch, hash); err != nil {
			return fmt.Errorf("store root head: %w", err)
		}
	}
	// Validation and batch preparation may outlive backendCtx. Check again before
	// committing so an already canceled import leaves DB and runtime tip unchanged.
	// Once the batch succeeds, publish the tip without another cancellation check.
	if err := c.backendCtx.Err(); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("commit root block: %w", err)
	}
	c.blockCache.Add(hash, block)
	if updateTip {
		c.tip.Store(header)
	}
	// Keep serialization through delivery so another insertion cannot overtake
	// this block's broadcast.
	// TODO(recovery): Init sends only the persisted canonical tip, so a shard
	// missing a side-chain root after interrupted delivery may reject its children
	// after restart. Persist pending delivery atomically with the local commit,
	// clear it only after all shards acknowledge, and replay it during Init before
	// publishing Ready, including when the pending block did not update the tip.
	if err := c.client.BroadcastRootBlock(c.backendCtx, block); err != nil {
		// The communication layer requests master shutdown on connection failure.
		// Some shards may already have advanced. Prevent further writes and
		// reinitialization until the cluster restarts from the persisted tip.
		c.stopping.Store(true)
		return fmt.Errorf("broadcast root block: %w", err)
	}
	return nil
}

// SetHead is reserved for rewinding the canonical root head to the given height.
// TODO: Implement when a caller needs it and the use case is confirmed.
// Serialize with chainmu and use rewriteCanonical to atomically update
// canonical indices and the persisted head, retaining blocks and confirmations.
// Coordinate forced shard switching and wait for all acknowledgements before
// resuming cluster work; return an error and stop further writes on failure.
func (c *RootChain) SetHead(number uint32) error {
	return ErrNotImplemented
}

// rewriteCanonical stages only changed indices, walking back to the shared
// canonical ancestor. It does not acquire chainmu or commit the batch.
// The caller must hold chainmu until the batch is committed and the runtime
// tip is updated.
func (c *RootChain) rewriteCanonical(batch ethdb.KeyValueWriter, header *types.RootBlockHeader) error {
	// The persisted head bounds canonical indices above a shorter, heavier tip.
	headHash, err := rawdb.ReadRootHeadBlockHash(c.db)
	if err != nil {
		return fmt.Errorf("read persisted root head: %w", err)
	}
	head, err := c.GetRootHeader(headHash)
	if err != nil {
		return err
	}
	if head == nil {
		return fmt.Errorf("%w: persisted root head", ErrRootDataMissing)
	}
	for number := uint64(header.Number) + 1; number <= uint64(head.Number); number++ {
		if err := rawdb.DeleteRootCanonicalHash(batch, uint32(number)); err != nil {
			return fmt.Errorf("delete canonical root %d: %w", number, err)
		}
	}
	for {
		if header.Number <= head.Number {
			hash := rawdb.ReadRootCanonicalHash(c.db, header.Number)
			if hash == header.Hash() {
				return nil
			}
		}
		if header.Number == 0 {
			return errors.New("root branch does not share canonical genesis")
		}
		if err := rawdb.WriteRootCanonicalHash(batch, header.Hash(), header.Number); err != nil {
			return fmt.Errorf("store canonical root %d: %w", header.Number, err)
		}
		parent, err := c.GetRootHeader(header.ParentHash)
		if err != nil {
			return err
		}
		if parent == nil {
			return fmt.Errorf("%w: canonical ancestor %x", ErrRootDataMissing, header.ParentHash)
		}
		if parent.Number != header.Number-1 {
			return errors.New("invalid root ancestor height")
		}
		header = parent
	}
}

// Close marks the chain as stopping, then waits for chain writes to finish and
// prevents further writes. Ready becomes false immediately; initialization stays
// recorded. It is safe to call repeatedly or concurrently.
// The backend must cancel backendCtx and stop new requests, cancel other RPC
// contexts and close cluster
// connections before calling Close, so pending RPC waits and network I/O finish.
// Close waits only for Init and InsertBlock; the owner must also wait for request
// handlers and other readers before closing the shared engine and database.
// Context cancellation belongs to the backend; Close does not request shutdown.
func (c *RootChain) Close() {
	c.closeOnce.Do(func() {
		c.stopping.Store(true)
		c.chainmu.Close()
	})
}

// Ready reports whether Init completed, backendCtx is active, and shutdown has
// not begun, either from Close or a root-block delivery failure.
// Sync, mining and imports must wait for it.
func (c *RootChain) Ready() bool {
	return c.initialized.Load() && !c.stopping.Load() && c.backendCtx.Err() == nil
}

// RootTip returns the recovered local tip, including before Init.
// The returned header is read-only; callers must copy it before modification.
func (c *RootChain) RootTip() *types.RootBlockHeader {
	return c.tip.Load()
}

// CreateBlockToMine builds an unsigned, unsealed candidate on the initialized root tip.
// TODO(sync): Skip candidate construction while the root synchronizer is running.
func (c *RootChain) CreateBlockToMine(ctx context.Context, coinbase common.Address) (*types.RootBlock, error) {
	if !c.Ready() {
		return nil, errChainNotReady
	}
	// A pending shard RPC must stop on either request cancellation or shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.backendCtx, cancel)
	defer stop()
	return c.builder.BuildBlock(ctx, RootBlockArgs{Parent: c.RootTip(), Coinbase: coinbase})
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
