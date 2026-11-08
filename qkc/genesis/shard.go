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
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// buildShard derives QKC genesis fields and commits the validated native
// specification in memory. Its consensus configuration comes from the JSON file.
func buildShard(spec *core.Genesis) (*Shard, error) {
	if err := prepareShard(spec); err != nil {
		return nil, err
	}
	// Commit validates the allocation and returns errors while constructing the
	// native trie/block. Both databases are temporary and never touch datadirs.
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	tdb := triedb.NewDatabase(db, nil)
	defer tdb.Close()
	block, err := spec.Commit(db, tdb, nil)
	if err != nil {
		return nil, err
	}
	if err := block.Header().SanityCheckQKC(); err != nil {
		return nil, err
	}
	return &Shard{Genesis: spec, Block: block}, nil
}

// prepareShard checks QKC genesis fields and fills required extra data and code.
// Callers validate chain configuration before this and staking deployment afterwards.
func prepareShard(g *core.Genesis) error {
	cfg := g.Config
	if cfg.Clique != nil || cfg.TerminalTotalDifficulty != nil || cfg.MergeNetsplitBlock != nil {
		return errors.New("QKC genesis cannot select Clique or an Ethereum Merge transition")
	}
	if cfg.EnableUBTAtGenesis || cfg.IsUBT(common.Big0, g.Timestamp) {
		return errors.New("QKC genesis does not support UBT initialization")
	}
	if g.Number != 0 || g.ParentHash != (common.Hash{}) || g.GasUsed != 0 {
		return errors.New("genesis number, parentHash and gasUsed must be zero")
	}
	if g.Nonce != 0 || g.Difficulty == nil || g.Difficulty.Sign() != 0 {
		return errors.New("QKC genesis difficulty and nonce must be zero; PoW difficulty is encoded in mixHash")
	}
	// QKC fixes the gas limit at genesis, so every block must fit a deposit.
	minGasLimit := max(params.MinGasLimit, params.XShardSendGas*params.XShardGasLimitDivisor)
	if g.GasLimit < minGasLimit || g.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("gasLimit must be between %d and %d", minGasLimit, params.MaxGasLimit)
	}
	if g.BaseFee != nil && (!cfg.IsLondon(common.Big0) || g.BaseFee.Sign() < 0 || g.BaseFee.BitLen() > 256) {
		return errors.New("baseFeePerGas requires London and a nonnegative uint256 value")
	}
	if (g.ExcessBlobGas != nil && (*g.ExcessBlobGas != 0 || !cfg.IsCancun(common.Big0, g.Timestamp))) ||
		(g.BlobGasUsed != nil && (*g.BlobGasUsed != 0 || !cfg.IsCancun(common.Big0, g.Timestamp))) {
		return errors.New("QKC genesis blob gas fields require Cancun and must be zero")
	}
	if g.SlotNumber != nil && (*g.SlotNumber != 0 || !cfg.IsAmsterdam(common.Big0, g.Timestamp)) {
		return errors.New("QKC genesis slotNumber requires Amsterdam and must be zero")
	}
	minimum := cfg.QuarkChain.PoSW.MinimumDifficulty
	if g.Mixhash == (common.Hash{}) {
		return errors.New("genesis mixHash is required and must be nonzero (QKC PoW difficulty)")
	}
	if new(big.Int).SetBytes(g.Mixhash[:]).Cmp(minimum) < 0 {
		return errors.New("genesis PoW difficulty in mixHash is below MinimumDifficulty")
	}
	extra := &types.QKCExtra{
		ChainID:        new(big.Int).Set(cfg.ChainID),
		Cursor:         types.XShardCursor{RootBlockHeight: 1},
		XShardGasLimit: g.GasLimit / params.XShardGasLimitDivisor,
	}
	if len(g.ExtraData) != 0 {
		supplied, err := types.DecodeQKCExtra(g.ExtraData)
		if err != nil {
			return err
		}
		if supplied.Version != 0 || supplied.ChainID.Cmp(cfg.ChainID) != 0 || supplied.PrevRootBlock != (common.Hash{}) ||
			supplied.Cursor != extra.Cursor || supplied.XShardGasUsed != 0 || supplied.XShardGasLimit != extra.XShardGasLimit {
			return errors.New("extraData conflicts with shard genesis identity, root reference, cursor or gas limits")
		}
		extra.Nonce, extra.MixHash = supplied.Nonce, supplied.MixHash
	}
	g.ExtraData = extra.Encode()
	if g.Alloc == nil {
		g.Alloc = make(types.GenesisAlloc)
	}
	for address, account := range g.Alloc {
		if account.Balance == nil || account.Balance.Sign() < 0 || account.Balance.BitLen() > 256 {
			return fmt.Errorf("allocation %s: balance must be a nonnegative uint256", address)
		}
	}
	required := core.PoSWStakingContractAllocs(cfg)
	system := core.SystemContractAllocs()
	// ActiveSystemContracts assumes London has passed, while a genesis may
	// still schedule London at a future block.
	if cfg.IsLondon(common.Big0) {
		for _, address := range cfg.ActiveSystemContracts(g.Timestamp) {
			if address == (common.Address{}) {
				continue
			}
			if account, ok := system[address]; ok {
				required[address] = account
			} else if len(g.Alloc[address].Code) == 0 {
				return fmt.Errorf("system contract %s must supply genesis code", address)
			}
		}
	}
	if cfg.IsOsaka(common.Big0, g.Timestamp) {
		required[params.DeterministicFactoryAddress] = system[params.DeterministicFactoryAddress]
	}
	for address, canonical := range required {
		account, exists := g.Alloc[address]
		if !exists {
			canonical.Code = bytes.Clone(canonical.Code)
			canonical.Balance = new(big.Int).Set(canonical.Balance)
			g.Alloc[address] = canonical
			continue
		}
		if len(account.Code) != 0 && !bytes.Equal(account.Code, canonical.Code) {
			return fmt.Errorf("system contract %s has conflicting genesis code", address)
		}
		account.Code = bytes.Clone(canonical.Code)
		g.Alloc[address] = account
	}
	return nil
}
