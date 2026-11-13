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

package poswroot

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// RootChainReader supplies read-only data for root consensus. Hash queries must
// also work on side branches. Errors indicate failed reads; nil values without an
// error mean a record is absent. The engine does not write confirmation records.
type RootChainReader interface {
	// GetRootHeader retrieves a root header by hash.
	// RootChain implementations should reuse their block/header cache here,
	// as pyquarkchain's RootDb does.
	GetRootHeader(hash common.Hash) (*qkctypes.RootBlockHeader, error)

	// GetLastConfirmedShardBlocks returns each shard's latest confirmed block
	// hash at rootHash, including confirmations inherited from earlier roots.
	// Keys are decimal chain IDs (chainID.String()). The map is read-only.
	GetLastConfirmedShardBlocks(rootHash common.Hash) (map[string]common.Hash, error)

	// GetShardBlockCoinbaseAmount returns after-tax miner income by block hash.
	// The amount is available only after an authorized node for the configured
	// shard has validated and successfully inserted the block. It need not be
	// canonical. Nil means an unknown block; zero income uses a non-nil zero value.
	GetShardBlockCoinbaseAmount(hash common.Hash) (*uint256.Int, error)

	// GetRootChainStakes reads the coinbase's locked stakes and allowed signer
	// at exactly shardBlockHash on chainID. Zero stakes use a non-nil zero value.
	GetRootChainStakes(chainID *big.Int, shardBlockHash common.Hash, coinbase common.Address) (*uint256.Int, common.Address, error)
}

// RootEngine defines consensus for root blocks, separate from consensus.Engine.
type RootEngine interface {
	// Author returns the root header's Coinbase address.
	Author(header *qkctypes.RootBlockHeader) (common.Address, error)

	// VerifyBlock checks a non-genesis root's header, body and seal against the
	// consensus rules without changing the block or stored chain data.
	VerifyBlock(chain RootChainReader, block *qkctypes.RootBlock) error

	// Prepare calculates and sets Difficulty and TotalDifficulty in the header.
	// These base values do not include any staking discount used for the seal.
	Prepare(chain RootChainReader, header *qkctypes.RootBlockHeader) error

	// Finalize prepares a root block for mining by setting CoinbaseAmount and
	// recomputing ShardHeaderHash, keeping the existing Coinbase address. It
	// updates and returns the input block; errors leave the block unchanged.
	Finalize(chain RootChainReader, block *qkctypes.RootBlock) (*qkctypes.RootBlock, error)

	// SealHash returns the root header hash used for mining and signing,
	// excluding the nonce, mix digest and signature.
	SealHash(header *qkctypes.RootBlockHeader) common.Hash

	// CalcDifficulty returns the base difficulty for a new root at timestamp
	// following parent, before any staking discount.
	CalcDifficulty(timestamp uint64, parent *qkctypes.RootBlockHeader) *big.Int

	// Close releases resources held by the root consensus engine.
	Close() error
}
