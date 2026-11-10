// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package consensus implements different Ethereum consensus engines.
package consensus

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// -----------------------------------------------------------------------------
// Shard consensus
// -----------------------------------------------------------------------------

// ChainHeaderReader defines a small collection of methods needed to access the local
// blockchain during header verification.
type ChainHeaderReader interface {
	// Config retrieves the blockchain's chain configuration.
	Config() *params.ChainConfig

	// CurrentHeader retrieves the current header from the local chain.
	CurrentHeader() *types.Header

	// GetHeader retrieves a block header from the database by hash and number.
	GetHeader(hash common.Hash, number uint64) *types.Header

	// GetHeaderByNumber retrieves a block header from the database by number.
	GetHeaderByNumber(number uint64) *types.Header

	// GetHeaderByHash retrieves a block header from the database by its hash.
	GetHeaderByHash(hash common.Hash) *types.Header
}

// ChainReader defines a small collection of methods needed to access the local
// blockchain during header and/or uncle verification.
type ChainReader interface {
	ChainHeaderReader

	// GetBlock retrieves a block from the database by hash and number.
	GetBlock(hash common.Hash, number uint64) *types.Block
}

// StakingReader provides read-only shard staking data at a specified block's state.
// It is separate from ChainHeaderReader because a header-only chain has no state.
type StakingReader interface {
	ChainReader
	GetShardLockedStakes(header *types.Header, staker common.Address) (*uint256.Int, error)
}

// SealVerifier extends engines whose seals depend on execution state. ValidateBody
// calls it after checking the parent state; such engines must not verify seals in
// VerifyHeaders. Other engines retain their existing header-verification behavior.
type SealVerifier interface {
	VerifySeal(chain ChainHeaderReader, header *types.Header) error
}

// Engine is an algorithm agnostic consensus engine.
type Engine interface {
	// Author retrieves the Ethereum address of the account that minted the given
	// block, which may be different from the header's coinbase if a consensus
	// engine is based on signatures.
	Author(header *types.Header) (common.Address, error)

	// VerifyHeader checks whether a header conforms to the consensus rules of a
	// given engine.
	VerifyHeader(chain ChainHeaderReader, header *types.Header) error

	// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
	// concurrently. The method returns a quit channel to abort the operations and
	// a results channel to retrieve the async verifications (the order is that of
	// the input slice).
	VerifyHeaders(chain ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error)

	// VerifyUncles verifies that the given block's uncles conform to the consensus
	// rules of a given engine.
	VerifyUncles(chain ChainReader, block *types.Block) error

	// Prepare initializes the consensus fields of a block header according to the
	// rules of a particular engine. The changes are executed inline.
	Prepare(chain ChainHeaderReader, header *types.Header) error

	// Finalize runs any post-transaction consensus-specific state modifications
	// (e.g. block rewards) but does not assemble the block.
	//
	// Note: The state database might be updated to reflect any consensus rules
	// that happen at finalization (e.g. block rewards).
	//
	// TODO(rjl) Integrate the state mutation into core.PostExecution.
	Finalize(chain ChainHeaderReader, header *types.Header, state vm.StateDB, body *types.Body)

	// Seal generates a new sealing request for the given input block and pushes
	// the result into the given channel.
	//
	// Note, the method returns immediately and will send the result async. More
	// than one result may also be returned depending on the consensus algorithm.
	Seal(chain ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error

	// SealHash returns the hash of a block prior to it being sealed.
	SealHash(header *types.Header) common.Hash

	// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
	// that a new block should have.
	CalcDifficulty(chain ChainHeaderReader, time uint64, parent *types.Header) *big.Int

	// Close terminates any background threads maintained by the consensus engine.
	Close() error
}

// -----------------------------------------------------------------------------
// Root consensus
// -----------------------------------------------------------------------------

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

// RootEngine defines consensus for root blocks, separate from the shard Engine.
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
