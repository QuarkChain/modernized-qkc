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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// rootGenesis contains the root consensus configuration and fixed seed fields.
// The root is not an EVM chain and has no native genesis allocation.
type rootGenesis struct {
	Config     *config.RootConfig    `json:"config"`
	Timestamp  *math.HexOrDecimal64  `json:"timestamp"`
	Difficulty *math.HexOrDecimal256 `json:"difficulty"`
	Coinbase   common.Address        `json:"coinbase"`
	ExtraData  hexutil.Bytes         `json:"extraData"`
	Nonce      math.HexOrDecimal64   `json:"nonce"`
	MixHash    common.Hash           `json:"mixHash"`
}

func readRoot(file string) (*rootGenesis, error) {
	var seed rootGenesis
	if err := decodeJSON(file, &seed); err != nil {
		return nil, err
	}
	return &seed, nil
}

// buildRoot constructs R0 from fixed seed fields and the validated G0 headers in
// network order. The genesis root has no parent, subsidy, signature or EVM state.
func buildRoot(file string, seed *rootGenesis, headers []*types.Header) (*qkctypes.RootBlock, error) {
	if seed.Timestamp == nil {
		return nil, fmt.Errorf("%s: root genesis timestamp is required", file)
	}
	if seed.Difficulty == nil || (*big.Int)(seed.Difficulty).Sign() <= 0 {
		return nil, fmt.Errorf("%s: root genesis difficulty must be a positive uint256", file)
	}
	difficulty, overflow := uint256.FromBig((*big.Int)(seed.Difficulty))
	if overflow {
		return nil, fmt.Errorf("%s: root genesis difficulty exceeds uint256", file)
	}
	if seed.Config == nil || seed.Config.PoSW == nil || seed.Config.PoSW.MinimumDifficulty == nil {
		return nil, fmt.Errorf("%s: root PoSW minimum difficulty is required", file)
	}
	if difficulty.ToBig().Cmp(seed.Config.PoSW.MinimumDifficulty) < 0 {
		return nil, fmt.Errorf("%s: root genesis difficulty is below MinimumDifficulty", file)
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("%s: root genesis requires shard genesis headers", file)
	}
	for i, header := range headers {
		if header == nil {
			return nil, fmt.Errorf("%s: nil shard genesis header at index %d", file, i)
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
