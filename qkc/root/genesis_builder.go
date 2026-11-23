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
	"fmt"

	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// BuildRootGenesis derives R0 from the native shard allocations without opening any node data directory.
func BuildRootGenesis(cfg *config.RootConfig) (*qkctypes.RootBlock, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	seed := cfg.Genesis
	headers := make(qkctypes.ShardBlockHeaders, len(cfg.Shards))
	for i, shard := range cfg.Shards {
		// Validate has checked the genesis, so ToBlock cannot fail on a bad allocation.
		header := shard.Genesis.ToBlock().Header()
		if err := header.SanityCheckQKC(); err != nil {
			return nil, fmt.Errorf("shard %s genesis: %w", seed.Config.ChainIDs[i], err)
		}
		headers[i] = header
	}
	difficulty := uint256.MustFromBig(seed.Difficulty)
	return qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Time: seed.Timestamp, Coinbase: seed.Coinbase,
		Difficulty: *difficulty, TotalDifficulty: *difficulty,
	}, headers, nil), nil
}
