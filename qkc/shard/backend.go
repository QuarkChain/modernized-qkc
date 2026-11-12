// Copyright 2026 The quarkchain Authors
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

package shard

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
	"golang.org/x/sync/errgroup"
)

var _ MinerBackend = (*Shard)(nil)

// Shard imports blocks, sends cross-shard lists and reports headers to master.
// ShardChain handles chain rules, root confirmations, and head selection.
//
// Ethereum owns the chain and cluster client. It must stop incoming requests
// and wait for in-flight calls before closing those shared resources.
type Shard struct {
	chain         *ShardChain
	clusterClient ClusterClient
}

// minedBlockDeliveryTimeout bounds RPC delivery for mining submissions.
const minedBlockDeliveryTimeout = 30 * time.Second

// New binds the existing chain and client without taking ownership of them.
// Init separately selects the root checkpoint supplied by master.
func New(chain *ShardChain, client ClusterClient) (*Shard, error) {
	if chain == nil || chain.chain == nil {
		return nil, errors.New("missing shard chain")
	}
	if chain.chain.Config().QuarkChain == nil {
		return nil, errNotQuarkChain
	}
	if client == nil {
		return nil, errors.New("missing cluster client")
	}
	return &Shard{chain: chain, clusterClient: client}, nil
}

// Init uses the checkpoint received from master to set the root tip and restore
// the shard head to its last confirmed block. Root-chain recovery is responsible
// for selecting the checkpoint before calling Init. On restart, pyquarkchain's
// RootDb.__recover_from_db selects the parent of the persisted non-genesis root
// tip, since the latest root may not have reached every shard.
func (s *Shard) Init(root *qkctypes.RootBlock) error {
	// TODO: Implement the one-block rewind in RootChain restart recovery and
	// have master pass the recovered checkpoint here. This is RootChain's work.
	return s.chain.Init(root)
}

// CreateBlockToMine builds an unsealed candidate on the shard head, paying
// coinbase, and returns the adjusted difficulty its seal must meet.
// It reuses the chain's bound builder. Like pyquarkchain, the timestamp follows
// the parent even when its next second is ahead of the clock.
func (s *Shard) CreateBlockToMine(ctx context.Context, coinbase common.Address) (*types.Block, *big.Int, error) {
	bc := s.chain.chain
	parent := bc.CurrentBlock()
	block, err := s.chain.BuildBlock(ctx, miner.QKCBlockArgs{
		Parent:       parent.Hash(),
		Timestamp:    max(uint64(time.Now().Unix()), parent.Time+1),
		FeeRecipient: coinbase,
	})
	if err != nil {
		return nil, nil, err
	}
	diff, err := posw.GetAdjustedDifficulty(bc.Config().QuarkChain.PoSW, bc, block.Header())
	if err != nil {
		return nil, nil, err
	}
	return block, diff, nil
}

// InsertMinedBlock imports and propagates a sealed candidate through the same
// path as a block received from the network.
func (s *Shard) InsertMinedBlock(block *types.Block) error {
	ctx, cancel := context.WithTimeout(context.Background(), minedBlockDeliveryTimeout)
	defer cancel()
	return s.AddShardBlock(ctx, block)
}

// SubscribeChainHeadEvent registers a subscription for shard head changes.
func (s *Shard) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return s.chain.chain.SubscribeChainHeadEvent(ch)
}

// AddRootBlock imports a root and reports whether it became the root tip.
func (s *Shard) AddRootBlock(root *qkctypes.RootBlock) (bool, error) {
	if root == nil {
		return false, fmt.Errorf("%w: nil root block", ErrInvalidRootBlock)
	}
	return s.chain.InsertRootBlock(root)
}

// BroadcastXShardTxList delivers an imported block's deposits to every other
// shard in config, sending an empty list when it has no deposits for that shard.
// Empty lists mark the source block as received so destinations can process a
// root that confirms it. Destinations come from config, not from emitted messages.
// Deliveries run concurrently, and every call must succeed for all hosts serving
// that shard before reporting to master. A failure cancels outstanding calls;
// all calls finish before returning. Retrying repeats all deliveries by block hash.
func (s *Shard) BroadcastXShardTxList(ctx context.Context, blockHash common.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deposits, err := XShardDeposits(s.chain.chain, blockHash)
	if err != nil {
		return err
	}
	config := s.chain.chain.Config()
	// Wait for all deliveries before reporting to master. A delivery failure
	// cancels the other deliveries through a child context; the caller's context
	// is unaffected, and Wait returns the first error after all tasks finish.
	group, ctx := errgroup.WithContext(ctx)
	for _, chainID := range config.QuarkChain.ChainIDs {
		if chainID.Cmp(config.ChainID) == 0 {
			continue
		}
		group.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			list := deposits[chainID.String()]
			if list == nil {
				list = []*types.DepositTx{}
			}
			if err := s.clusterClient.BroadcastXShardTxList(ctx, chainID, blockHash, list); err != nil {
				return fmt.Errorf("broadcast block %x deposits to shard %d: %w", blockHash, chainID, err)
			}
			return nil
		})
	}
	return group.Wait()
}

// AddShardBlock imports a block, delivers all cross-shard lists, then reports
// its header and after-tax coinbase amount to master, matching pyquarkchain's
// propagation order. This method waits for every destination to acknowledge its
// list, including empty lists, before reporting the header, because a root may
// confirm it afterward.
//
// Most errors can be resolved through retries or recovery. The main retry
// paths are:
//
//  1. After an error, the caller calls AddShardBlock again with the same block.
//  2. Once root sync is added, it can call AddShardBlock for the shard blocks
//     needed before importing a root block.
//
// Retry results by failed stage:
//
//   - Invalid block: discard it without retrying. If required data is missing,
//     the caller can sync that data and try again.
//   - Storage succeeds but SetCanonical fails: a retry skips the known block and
//     continues broadcasting and reporting without calling SetCanonical again.
//     A later descendant selected as head, or a root confirming this branch,
//     may make the stored block canonical.
//   - Import succeeds but broadcasting fails: a retry skips import, sends all
//     cross-shard lists again, and reports to master after all sends succeed.
//     Recipients accept repeated inserts.
//   - Reporting or the commit-marker write fails: a retry sends all lists and
//     reports to master again. Master may already have accepted the earlier
//     report, so recipients must accept duplicate requests.
//
// A persisted commit marker skips completed blocks, including after restart.
// Concurrent calls may repeat delivery before the marker is stored; recipients
// must handle duplicates just as they do for retries.
// TODO(sync): Network handlers must skip only committed blocks, so receiving an
// uncommitted known block retries delivery. Also retry uncommitted blocks in the
// unconfirmed range in a background loop, including failed mined blocks and
// single-cluster setups where root-sync recovery cannot trigger a retry.
func (s *Shard) AddShardBlock(ctx context.Context, block *types.Block) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if block == nil {
		return errors.New("nil shard block")
	}
	// Skip completed deliveries so replaying old blocks cannot repeatedly amplify
	// traffic into cross-shard broadcasts and master reports.
	committed, err := rawdb.IsShardBlockCommitted(s.chain.db, block.Hash())
	if err != nil {
		return fmt.Errorf("read block %x commit marker: %w", block.Hash(), err)
	}
	if committed {
		return nil
	}
	if err := s.chain.InsertShardBlock(block); err != nil {
		return err
	}
	// Resolve local reporting data before sending RPCs, so calculation failures
	// do not repeat cross-shard delivery on every retry.
	coinbaseAmount, err := s.chain.chain.GetCoinbaseAmount(block.Hash())
	if err != nil {
		return err
	}
	if err := s.BroadcastXShardTxList(ctx, block.Hash()); err != nil {
		return err
	}
	if err := s.clusterClient.SendShardBlockHeaderToMaster(ctx, block.Header(), coinbaseAmount); err != nil {
		return fmt.Errorf("report block %x to master: %w", block.Hash(), err)
	}
	if err := rawdb.WriteShardBlockCommit(s.chain.db, block.Hash()); err != nil {
		return fmt.Errorf("commit block %x delivery: %w", block.Hash(), err)
	}
	return nil
}

// CurrentBlock returns the canonical shard head.
func (s *Shard) CurrentBlock() *types.Header {
	return s.chain.chain.CurrentBlock()
}

// RootTip returns the root-tip hash, or zero before Init, for mining compatibility.
func (s *Shard) RootTip() common.Hash {
	if tip := s.chain.RootTip(); tip != nil {
		return tip.Hash()
	}
	return common.Hash{}
}

// GetRootChainStakes reads stakes at the requested shard block. Only the first
// configured chain serves root-chain stakes; the blockchain enforces that restriction.
func (s *Shard) GetRootChainStakes(address common.Address, shardBlockHash common.Hash) (*uint256.Int, common.Address, error) {
	header := s.chain.chain.GetHeaderByHash(shardBlockHash)
	if header == nil {
		return nil, common.Address{}, fmt.Errorf("shard block header %s not found", shardBlockHash.Hex())
	}
	return s.chain.chain.GetRootChainStakes(header, address)
}

// AddXshardTxList stores a validated remote block's deposits. An empty list is
// still RLP-encoded and written, distinguishing it from a list not yet received.
// The remote shard validates the list; repeated deliveries overwrite the same key.
func (s *Shard) AddXshardTxList(shardBlockHash common.Hash, deposits []*types.DepositTx) error {
	if err := rawdb.WriteXShardDepositsWithError(s.chain.db, shardBlockHash, deposits); err != nil {
		return fmt.Errorf("store block %x cross-shard deposits: %w", shardBlockHash, err)
	}
	return nil
}

// GetUnconfirmedHeaderList returns the earliest unconfirmed canonical headers
// selected by ShardChain, independently of propagation status.
func (s *Shard) GetUnconfirmedHeaderList() ([]*types.Header, error) {
	return s.chain.GetUnconfirmedHeaderList()
}
