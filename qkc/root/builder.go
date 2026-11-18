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

// Package root manages the root chain.
package root

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/master"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// errInconsistentHeaders reports shard headers from the cluster that are not an
// ordered, linked list of one shard's blocks. The candidate must be built again.
var errInconsistentHeaders = errors.New("inconsistent unconfirmed shard headers")

// BlockArgs specifies a root candidate. Parent is required. A zero Timestamp
// selects the later of the current time and one second after the parent, as
// pyquarkchain does.
type BlockArgs struct {
	Parent    common.Hash
	Timestamp uint64
	Coinbase  common.Address
}

// Builder creates unsealed root candidates from the unconfirmed shard headers
// collected from the cluster, as pyquarkchain's __create_root_block_to_mine.
// config must be the configuration the engine was created with. The builder
// does not own the engine or the client.
type Builder struct {
	config   *params.QuarkChainConfig
	engine   poswroot.RootEngine
	client   master.ClusterClient
	chainIDs []*big.Int // Configured chain IDs in the ascending order root bodies use.
}

// NewBuilder binds the root consensus engine and the cluster client that
// supplies shard headers.
func NewBuilder(config *params.QuarkChainConfig, engine poswroot.RootEngine, client master.ClusterClient) (*Builder, error) {
	if config == nil || engine == nil || client == nil {
		return nil, errors.New("missing root configuration, engine or cluster client")
	}
	chainIDs := slices.Clone(config.ChainIDs)
	slices.SortFunc(chainIDs, (*big.Int).Cmp)
	return &Builder{config: config, engine: engine, client: client, chainIDs: chainIDs}, nil
}

// BuildBlock creates one unsealed root candidate on args.Parent, read through
// chain. The engine prepares the header before the cluster is queried. For each
// configured shard, in ascending chain ID order, the candidate includes the
// collected headers that continue the parent's last confirmation, as selected
// by selectShardHeaders. The engine then sets the reward and shard commitment.
// The tracking data is left empty. The caller seals and imports the result.
func (b *Builder) BuildBlock(ctx context.Context, chain poswroot.RootChainReader, args BlockArgs) (*qkctypes.RootBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if chain == nil {
		return nil, errors.New("missing root chain reader")
	}
	parent, err := chain.GetRootHeader(args.Parent)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, consensus.ErrUnknownAncestor
	}
	timestamp := args.Timestamp
	if timestamp == 0 {
		timestamp = max(uint64(time.Now().Unix()), parent.Time+1)
	}
	header := &qkctypes.RootBlockHeader{
		Number:     parent.Number + 1,
		ParentHash: args.Parent,
		Coinbase:   args.Coinbase,
		Time:       timestamp,
	}
	// Prepare checks the parent link and timestamp, then sets the difficulty.
	if err := b.engine.Prepare(chain, header); err != nil {
		return nil, err
	}
	candidates, err := b.client.GetUnconfirmedHeaderList(ctx)
	if err != nil {
		return nil, fmt.Errorf("collect unconfirmed shard headers: %w", err)
	}
	confirmed, err := chain.GetLastConfirmedShardBlocks(args.Parent)
	if err != nil {
		return nil, err
	}
	branch := newRootBranch(chain, parent)
	var headers qkctypes.ShardBlockHeaders
	for _, id := range b.chainIDs {
		selected, err := b.selectShardHeaders(chain, branch, id, candidates[id.String()], confirmed[id.String()], timestamp)
		if err != nil {
			return nil, err
		}
		headers = append(headers, selected...)
	}
	return b.engine.Finalize(chain, qkctypes.NewRootBlock(header, headers, nil))
}

// selectShardHeaders returns the usable headers from one shard's collected list.
//
// The list must belong to shard id and be linked by height and hash, otherwise
// an error is returned, like pyquarkchain's height check. Headers already
// confirmed by the parent are skipped. If the rest does not continue the
// parent's confirmation, the shard answered from another root tip, and none of
// its headers are included. The selection stops at MaxBlocksPerRootBlock and
// before the first header created after rootTime, unknown to the root chain,
// or referencing a root outside the parent's branch, so the candidate passes
// root body validation.
func (b *Builder) selectShardHeaders(chain poswroot.RootChainReader, branch *rootBranch, id *big.Int, candidates []*types.Header, confirmed common.Hash, rootTime uint64) ([]*types.Header, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	hashes := make([]common.Hash, len(candidates))
	prevRoots := make([]common.Hash, len(candidates))
	for i, header := range candidates {
		if header == nil || header.Number == nil {
			return nil, fmt.Errorf("%w: chain %s: missing header %d", errInconsistentHeaders, id, i)
		}
		extra, err := header.QKCExtra()
		if err != nil || extra.ChainID.Cmp(id) != 0 {
			return nil, fmt.Errorf("%w: chain %s: header %d belongs to another chain", errInconsistentHeaders, id, i)
		}
		if i > 0 && (header.ParentHash != hashes[i-1] || header.Number.Cmp(new(big.Int).Add(candidates[i-1].Number, common.Big1)) != 0) {
			return nil, fmt.Errorf("%w: chain %s: header %d does not follow the previous one", errInconsistentHeaders, id, i)
		}
		hashes[i], prevRoots[i] = header.Hash(), extra.PrevRootBlock
	}
	if confirmed == (common.Hash{}) {
		return nil, fmt.Errorf("missing parent root confirmation of chain %s", id)
	}
	start := 0
	if i := slices.Index(hashes, confirmed); i >= 0 {
		start = i + 1
	} else if candidates[0].ParentHash != confirmed {
		log.Debug("Skipping shard headers not continuing the parent root", "chain", id, "confirmed", confirmed, "first", candidates[0].Number)
		return nil, nil
	}
	end := start
	for ; end < len(candidates) && uint64(end-start) < b.config.MaxBlocksPerRootBlock; end++ {
		if candidates[end].Time > rootTime {
			break
		}
		// Like pyquarkchain, stop at the first block master has not recorded.
		amount, err := chain.GetShardBlockCoinbaseAmount(hashes[end])
		if err != nil {
			return nil, err
		}
		if amount == nil {
			break
		}
		onBranch, err := branch.contains(prevRoots[end])
		if err != nil {
			return nil, err
		}
		if !onBranch {
			break
		}
	}
	return candidates[start:end], nil
}

// rootBranch answers whether roots are the parent or its ancestors. It walks
// parent links lazily, so each ancestor is read at most once per candidate.
type rootBranch struct {
	chain  poswroot.RootChainReader
	hashes map[common.Hash]bool // Roots from the parent down to lowest.
	lowest *qkctypes.RootBlockHeader
}

func newRootBranch(chain poswroot.RootChainReader, parent *qkctypes.RootBlockHeader) *rootBranch {
	return &rootBranch{chain: chain, hashes: map[common.Hash]bool{parent.Hash(): true}, lowest: parent}
}

// contains reports whether hash is on the branch. An unknown root is not.
func (r *rootBranch) contains(hash common.Hash) (bool, error) {
	if r.hashes[hash] {
		return true, nil
	}
	target, err := r.chain.GetRootHeader(hash)
	if err != nil || target == nil {
		return false, err
	}
	for r.lowest.Number > target.Number {
		parent, err := r.chain.GetRootHeader(r.lowest.ParentHash)
		if err != nil {
			return false, err
		}
		if parent == nil {
			return false, consensus.ErrUnknownAncestor
		}
		if parent.Number+1 != r.lowest.Number {
			return false, errors.New("inconsistent root ancestry")
		}
		r.hashes[r.lowest.ParentHash] = true
		r.lowest = parent
	}
	return r.hashes[hash], nil
}
