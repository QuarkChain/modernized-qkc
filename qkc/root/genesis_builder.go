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

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// BuildRootGenesis derives R0 from the native shard allocations without opening any node data directory.
func BuildRootGenesis(cfg *config.RootConfig) (*qkctypes.RootBlock, error) {
	if cfg == nil || cfg.Genesis == nil {
		return nil, errors.New("missing root genesis")
	}
	seed := cfg.Genesis
	if err := seed.Config.Check(); err != nil {
		return nil, err
	}
	if seed.Difficulty == nil || seed.Difficulty.BitLen() > 256 || seed.Difficulty.Cmp(seed.Config.PoSW.MinimumDifficulty) < 0 {
		return nil, errors.New("invalid root genesis difficulty")
	}
	headers, err := shardGenesisHeaders(cfg.ShardGeneses, seed.Config)
	if err != nil {
		return nil, err
	}
	difficulty := uint256.MustFromBig(seed.Difficulty)
	return qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Time: seed.Timestamp, Coinbase: seed.Coinbase,
		Difficulty: *difficulty, TotalDifficulty: *difficulty,
	}, headers, nil), nil
}

func shardGenesisHeaders(shards []*core.Genesis, rules *params.QuarkChainConfig) (qkctypes.ShardBlockHeaders, error) {
	if len(shards) != len(rules.ChainIDs) {
		return nil, errors.New("shard genesis count differs from configured chain IDs")
	}
	headers := make(qkctypes.ShardBlockHeaders, len(shards))
	for i, shard := range shards {
		id := rules.ChainIDs[i]
		if shard == nil || shard.Config == nil || shard.Config.ChainID == nil || shard.Config.ChainID.Cmp(id) != 0 || shard.Config.QuarkChain == nil {
			return nil, fmt.Errorf("missing shard genesis for chain %s", id)
		}
		if !shard.Config.QuarkChain.EqualNetworkRules(rules) {
			return nil, fmt.Errorf("shard %s network rules differ from root", id)
		}
		header, err := shardGenesisHeader(shard)
		if err != nil {
			return nil, fmt.Errorf("shard %s genesis: %w", id, err)
		}
		headers[i] = header
	}
	return headers, nil
}

func shardGenesisHeader(shard *core.Genesis) (*types.Header, error) {
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	var options *triedb.Config
	if shard.IsUBT() {
		options = triedb.UBTDefaults
	}
	trie := triedb.NewDatabase(db, options)
	defer trie.Close()
	block, err := shard.Commit(db, trie, nil)
	if err != nil {
		return nil, err
	}
	header := block.Header()
	return header, header.SanityCheckQKC()
}
