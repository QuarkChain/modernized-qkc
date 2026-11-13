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
	posw "github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

var (
	ErrRootDataMissing = errors.New("missing root chain data")
	ErrNotImplemented  = errors.New("root chain: function not implemented")
	errChainStopped    = errors.New("root chain is stopped")
)

// RootChain manages root-chain data and recovery using a caller-provided database,
// client and consensus engine. The caller owns their lifetimes. Configuration must
// match the engine and stay unchanged after New. The caller must drain in-flight
// operations before closing resources.
// TODO(backend): Call Init after cluster connections are ready and before
// starting sync, mining or imports.
type RootChain struct {
	config *params.QuarkChainConfig
	client ClusterClient
	db     ethdb.Database
	engine posw.RootEngine

	// Serialize chain writes, including Init. Readers use atomic state or the DB.
	chainmu   *syncx.ClosableMutex
	closeOnce sync.Once

	// Published headers are immutable; updates replace the tip pointer.
	tip   atomic.Pointer[types.RootBlockHeader]
	ready atomic.Bool
}

var _ posw.RootChainReader = (*RootChain)(nil)

// New recovers the persisted head's parent (or genesis) as the runtime tip,
// matching pyquarkchain's RootDb recovery without changing the stored head.
// It binds the supplied client and engine without taking ownership of them or
// making cluster RPCs. The caller must validate configuration when creating the engine.
// Genesis, canonical indices and cumulative confirmations must already be stored.
func New(config *params.QuarkChainConfig, client ClusterClient, db ethdb.Database, engine posw.RootEngine) (*RootChain, error) {
	if config == nil || client == nil || db == nil || engine == nil {
		return nil, errors.New("missing root configuration, cluster client, database or engine")
	}
	hash, err := rawdb.ReadRootHeadBlockHash(db)
	if err != nil {
		return nil, fmt.Errorf("read persisted root head: %w", err)
	}
	if hash == (common.Hash{}) {
		return nil, fmt.Errorf("%w: persisted root head", ErrRootDataMissing)
	}
	tip := rawdb.ReadRootBlockHeader(db, hash)
	if tip == nil {
		return nil, fmt.Errorf("%w: persisted root tip", ErrRootDataMissing)
	}
	if tip.Hash() != hash {
		return nil, fmt.Errorf("root header does not match stored hash %x", hash)
	}
	// Match pyquarkchain's RootDb recovery: use the persisted head's parent as
	// the runtime tip unless it is genesis, without rewriting the stored head.
	if tip.Number != 0 {
		hash = tip.ParentHash
	}
	checkpoint := rawdb.ReadRootBlock(db, hash)
	if checkpoint == nil {
		return nil, fmt.Errorf("%w: recovery checkpoint", ErrRootDataMissing)
	}
	if checkpoint.Hash() != hash {
		return nil, fmt.Errorf("root block does not match stored hash %x", hash)
	}
	c := &RootChain{
		config: config, client: client, db: db, engine: engine,
		chainmu: syncx.NewClosableMutex(),
	}
	if err := c.checkConfirmations(hash); err != nil {
		return nil, err
	}
	c.tip.Store(checkpoint.Header())
	return c, nil
}

// Recovery requires exactly one nonzero confirmation for every configured shard.
func (c *RootChain) checkConfirmations(hash common.Hash) error {
	confirmed, err := c.GetLastConfirmedShardBlocks(hash)
	if err != nil {
		return err
	}
	if len(confirmed) != len(c.config.ChainIDs) {
		return fmt.Errorf("root %x shard confirmation count mismatch: have %d, want %d", hash, len(confirmed), len(c.config.ChainIDs))
	}
	for _, id := range c.config.ChainIDs {
		if confirmed[id.String()] == (common.Hash{}) {
			return fmt.Errorf("%w: root %x chain %s confirmation", ErrRootDataMissing, hash, id)
		}
	}
	return nil
}

// Init initializes shards at the recovered tip, then replays pending root
// delivery. Connections must be ready before this call. Failure permits retry;
// repeating a successful Init is a no-op. Init does not change the local tip.
func (c *RootChain) Init(ctx context.Context) error {
	if !c.chainmu.TryLock() {
		return errChainStopped
	}
	defer c.chainmu.Unlock()
	if err := ctx.Err(); err != nil {
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
	// Initialize all shards at the recovered checkpoint before replaying root
	// blocks, matching pyquarkchain's __init_shards startup step.
	if err := c.client.Ping(ctx, block); err != nil {
		return fmt.Errorf("initialize shards: %w", err)
	}
	// A committing marker records root delivery that may have been interrupted.
	// Replay it after shard initialization; retain the marker if replay fails.
	pending, err := rawdb.ReadRootCommittingHash(c.db)
	if err != nil {
		return fmt.Errorf("read committing root: %w", err)
	}
	if pending != (common.Hash{}) {
		block, err := c.GetRootBlock(pending)
		if err != nil {
			return err
		}
		// Keep the marker if its block cannot be loaded or delivered.
		if block == nil {
			return fmt.Errorf("%w: committing root %x", ErrRootDataMissing, pending)
		}
		if err := c.client.BroadcastRootBlock(ctx, block); err != nil {
			return fmt.Errorf("replay root block: %w", err)
		}
		if err := rawdb.DeleteRootCommittingHash(c.db); err != nil {
			return fmt.Errorf("clear committing root: %w", err)
		}
	}
	c.ready.Store(true)
	return nil
}

// Close waits for chain writes to finish and prevents further writes. It is safe
// to call repeatedly or concurrently. The caller must cancel in-flight RPCs
// before calling Close, and drain readers before closing the shared resources.
func (c *RootChain) Close() {
	c.closeOnce.Do(func() {
		c.chainmu.Close()
		c.ready.Store(false)
	})
}

// Ready reports whether Init completed and the chain has not been closed.
// Sync, mining and imports must wait for it.
func (c *RootChain) Ready() bool { return c.ready.Load() }

// RootTip returns the recovered local tip, including before Init.
// The returned header is read-only; callers must copy it before modification.
func (c *RootChain) RootTip() *types.RootBlockHeader {
	return c.tip.Load()
}

// GetRootHeader reads a header by hash from any stored branch.
func (c *RootChain) GetRootHeader(hash common.Hash) (*types.RootBlockHeader, error) {
	header := rawdb.ReadRootBlockHeader(c.db, hash)
	if header != nil && header.Hash() != hash {
		return nil, fmt.Errorf("root header does not match stored hash %x", hash)
	}
	return header, nil
}

// GetRootBlock reads a block by hash from any stored branch.
func (c *RootChain) GetRootBlock(hash common.Hash) (*types.RootBlock, error) {
	block := rawdb.ReadRootBlock(c.db, hash)
	if block != nil && block.Hash() != hash {
		return nil, fmt.Errorf("root block does not match stored hash %x", hash)
	}
	return block, nil
}

// GetRootHeaderByNumber reads the stored canonical header.
func (c *RootChain) GetRootHeaderByNumber(number uint32) (*types.RootBlockHeader, error) {
	hash := rawdb.ReadRootCanonicalHash(c.db, number)
	if hash == (common.Hash{}) {
		return nil, nil
	}
	return c.GetRootHeader(hash)
}

// GetRootBlockByNumber follows the stored canonical index, which may include
// blocks above the recovery checkpoint.
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
