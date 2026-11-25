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

package root

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// RootGenesis holds the fixed R0 inputs and the effective root consensus rules.
type RootGenesis struct {
	Timestamp  uint64
	Difficulty *big.Int
	Coinbase   common.Address
	Config     *params.QuarkChainConfig
}

// Validate checks the root consensus rules and the fixed R0 difficulty.
func (g *RootGenesis) Validate() error {
	if g == nil || g.Config == nil {
		return errors.New("missing root genesis")
	}
	if g.Difficulty == nil || g.Difficulty.Sign() <= 0 || g.Difficulty.BitLen() > 256 {
		return errors.New("Difficulty must be a positive uint256")
	}
	if err := g.Config.ConsensusType.Check(); err != nil {
		return err
	}
	if err := g.Config.Check(); err != nil {
		return err
	}
	if g.Difficulty.Cmp(g.Config.PoSW.MinimumDifficulty) < 0 {
		return errors.New("Difficulty is below MinimumDifficulty")
	}
	return nil
}

// ValidateShardGenesis checks one shard's native genesis. When rules is set,
// the shard must also agree with the shared network rules.
func ValidateShardGenesis(shard *core.Genesis, rules *params.QuarkChainConfig) error {
	if shard == nil || shard.Config == nil || shard.Config.QuarkChain == nil {
		return errors.New("missing shard genesis or QuarkChain config")
	}
	if err := shard.ValidateQuarkChain(); err != nil {
		return err
	}
	if err := shard.Config.CheckConfigForkOrder(); err != nil {
		return err
	}
	if rules != nil && !shard.Config.QuarkChain.EqualNetworkRules(rules) {
		return errors.New("shard network rules differ: ChainIDs, RewardTaxRate and MaxBlocksPerRootBlock must agree")
	}
	return nil
}

// BuildRootGenesis derives R0 offline from shard geneses in ChainIDs order.
func BuildRootGenesis(seed *RootGenesis, shards []*core.Genesis) (*qkctypes.RootBlock, error) {
	if err := seed.Validate(); err != nil {
		return nil, err
	}
	rules := seed.Config
	if len(shards) != len(rules.ChainIDs) {
		return nil, errors.New("shard genesis count differs from configured chain IDs")
	}
	headers := make(qkctypes.ShardBlockHeaders, len(shards))
	for i, shard := range shards {
		id := rules.ChainIDs[i]
		if err := ValidateShardGenesis(shard, rules); err != nil {
			return nil, fmt.Errorf("shard %s genesis: %w", id, err)
		}
		if shard.Config.ChainID.Cmp(id) != 0 {
			return nil, fmt.Errorf("expected shard genesis for chain %s at index %d, got chain %s", id, i, shard.Config.ChainID)
		}
		if shard.Timestamp > seed.Timestamp {
			return nil, fmt.Errorf("shard %s genesis timestamp %d is later than root genesis timestamp %d", id, shard.Timestamp, seed.Timestamp)
		}
		header := shard.ToBlock().Header()
		if err := header.SanityCheckQKC(); err != nil {
			return nil, fmt.Errorf("shard %s genesis: %w", id, err)
		}
		headers[i] = header
	}
	difficulty := uint256.MustFromBig(seed.Difficulty)
	return qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Time: seed.Timestamp, Coinbase: seed.Coinbase,
		Difficulty: *difficulty, TotalDifficulty: *difficulty,
	}, headers, nil), nil
}
