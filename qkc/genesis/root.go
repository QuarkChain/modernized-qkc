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

package genesis

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// buildRoot constructs R0 from loaded seed fields and G0 headers in network order.
func buildRoot(seed *config.RootGenesis, headers []*types.Header) (*qkctypes.RootBlock, error) {
	if seed == nil || seed.Timestamp == nil || seed.Difficulty == nil {
		return nil, fmt.Errorf("loaded root genesis is required")
	}
	difficulty, overflow := uint256.FromBig((*big.Int)(seed.Difficulty))
	if overflow {
		return nil, fmt.Errorf("root genesis difficulty exceeds uint256")
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("root genesis requires shard genesis headers")
	}
	for i, header := range headers {
		if header == nil {
			return nil, fmt.Errorf("nil shard genesis header at index %d", i)
		}
	}
	return qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Coinbase:        seed.Coinbase,
		Time:            uint64(*seed.Timestamp),
		Difficulty:      *difficulty,
		TotalDifficulty: *difficulty,
		Extra:           seed.ExtraData,
		Nonce:           uint64(seed.Nonce),
		MixDigest:       seed.MixHash,
	}, qkctypes.ShardBlockHeaders(headers), nil), nil
}
