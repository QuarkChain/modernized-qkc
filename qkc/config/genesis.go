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

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// RootGenesis contains the root consensus configuration and fixed seed fields.
// The root is not an EVM chain and has no native genesis allocation.
type RootGenesis struct {
	*RootConfig `json:"config"`
	Timestamp   *math.HexOrDecimal64  `json:"timestamp"`
	Difficulty  *math.HexOrDecimal256 `json:"difficulty"`
	Coinbase    common.Address        `json:"coinbase"`
	ExtraData   hexutil.Bytes         `json:"extraData"`
	Nonce       math.HexOrDecimal64   `json:"nonce"`
	MixHash     common.Hash           `json:"mixHash"`
}

// LoadGenesis loads and prepares every genesis, then checks network membership.
func (c *ClusterConfig) LoadGenesis() error {
	if err := c.Master.LoadGenesis(); err != nil {
		return fmt.Errorf("Master: %w", err)
	}
	for i := range c.Slaves {
		if err := c.Slaves[i].LoadGenesis(); err != nil {
			return fmt.Errorf("Slaves[%d]: %w", i, err)
		}
	}
	return c.validateGenesisMembership()
}

// LoadGenesis reads and validates only this process's genesis and fills required
// QKC fields and contract code. It neither constructs blocks nor opens a database.
func (c *NodeConfig) LoadGenesis() error {
	if c.GenesisFile == "" {
		return errors.New("GenesisFile is required")
	}
	if c.Role != RoleMaster && c.Role != RoleSlave {
		return fmt.Errorf("invalid process role %q", c.Role)
	}
	if c.Role == RoleSlave && c.Eth == nil {
		return errors.New("slave requires execution settings")
	}
	if c.Role == RoleMaster {
		var genesis RootGenesis
		if err := decodeJSON(c.GenesisFile, &genesis); err != nil {
			return err
		}
		c.Root = &genesis
	} else {
		var genesis core.Genesis
		if err := decodeJSON(c.GenesisFile, &genesis); err != nil {
			return err
		}
		c.Eth.Genesis = &genesis
	}
	if err := c.validateGenesisConfig(); err != nil {
		return fmt.Errorf("%s: %w", c.GenesisFile, err)
	}
	var err error
	if c.Role == RoleMaster {
		err = c.Root.validate()
	} else {
		err = prepareShard(c.Eth.Genesis)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", c.GenesisFile, err)
	}
	return nil
}

func (c *NodeConfig) validateGenesisConfig() error {
	switch c.Role {
	case RoleMaster:
		if c.Root == nil || c.Root.RootConfig == nil {
			return errors.New("root Genesis config is required; set GenesisFile")
		}
		if err := c.Root.ConsensusType.Check(); err != nil {
			return err
		}
		if err := c.Root.PoSW.Check(); err != nil {
			return err
		}
	case RoleSlave:
		if c.ChainID == nil || c.Eth == nil {
			return errors.New("slave requires ChainID and execution settings")
		}
		if err := checkShardChainID(c.ChainID); err != nil {
			return err
		}
		if c.Eth.Genesis == nil || c.Eth.Genesis.Config == nil {
			return errors.New("shard Genesis config is required; set GenesisFile")
		}
		chain := c.Eth.Genesis.Config
		if err := checkShardChainID(chain.ChainID); err != nil {
			return fmt.Errorf("Genesis: %w", err)
		}
		if chain.ChainID.Cmp(c.ChainID) != 0 {
			return fmt.Errorf("Genesis ChainID %v does not match deployed ChainID %v", chain.ChainID, c.ChainID)
		}
		if chain.QuarkChain == nil {
			return errors.New("Genesis QuarkChain configuration is required")
		}
		if err := chain.QuarkChain.ConsensusType.Check(); err != nil {
			return err
		}
		if err := chain.CheckConfigForkOrder(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid process role %q", c.Role)
	}
	return nil
}

func (c *ClusterConfig) validateGenesisMembership() error {
	if c.Master.Role != RoleMaster || c.Master.ChainID != nil {
		return errors.New("Master must not specify ChainID")
	}
	if len(c.Slaves) == 0 {
		return errors.New("deployment must define at least one shard")
	}
	var first *params.ChainConfig
	ids := make(map[string]bool, len(c.Slaves))
	for i, cfg := range c.Slaves {
		if cfg.Role != RoleSlave {
			return fmt.Errorf("Slaves[%d]: invalid process role %q", i, cfg.Role)
		}
		if cfg.ChainID == nil {
			return fmt.Errorf("Slaves[%d]: ChainID is required", i)
		}
		id := cfg.ChainID.String()
		if ids[id] {
			return fmt.Errorf("duplicate deployed chain ID %v", cfg.ChainID)
		}
		ids[id] = true
		chain := cfg.Eth.Genesis.Config
		if first == nil {
			first = chain
		} else {
			if !slices.EqualFunc(first.QuarkChain.ChainIDs, chain.QuarkChain.ChainIDs, func(a, b *big.Int) bool { return a.Cmp(b) == 0 }) {
				return fmt.Errorf("chain %v: genesis chain IDs must match in order across shards", chain.ChainID)
			}
			if err := checkSharedForks(first, chain); err != nil {
				return err
			}
		}
	}
	if len(ids) != len(first.QuarkChain.ChainIDs) {
		return errors.New("deployment must contain every genesis chain ID")
	}
	return nil
}

func checkSharedForks(first, other *params.ChainConfig) error {
	for _, fork := range []struct {
		name string
		a, b *big.Int
	}{
		{"HomesteadBlock", first.HomesteadBlock, other.HomesteadBlock},
		{"EIP150Block", first.EIP150Block, other.EIP150Block},
		{"EIP155Block", first.EIP155Block, other.EIP155Block},
		{"EIP158Block", first.EIP158Block, other.EIP158Block},
		{"ByzantiumBlock", first.ByzantiumBlock, other.ByzantiumBlock},
		{"ConstantinopleBlock", first.ConstantinopleBlock, other.ConstantinopleBlock},
		{"PetersburgBlock", first.PetersburgBlock, other.PetersburgBlock},
		{"IstanbulBlock", first.IstanbulBlock, other.IstanbulBlock},
		{"BerlinBlock", first.BerlinBlock, other.BerlinBlock},
		{"LondonBlock", first.LondonBlock, other.LondonBlock},
	} {
		if (fork.a == nil) != (fork.b == nil) || (fork.a != nil && fork.b != nil && fork.a.Cmp(fork.b) != 0) {
			return fmt.Errorf("chain %v: genesis fork %s must match chain %v", other.ChainID, fork.name, first.ChainID)
		}
	}
	return nil
}

func (g *RootGenesis) validate() error {
	if g.Timestamp == nil {
		return errors.New("root genesis timestamp is required")
	}
	if g.Difficulty == nil || (*big.Int)(g.Difficulty).Sign() <= 0 || (*big.Int)(g.Difficulty).BitLen() > 256 {
		return errors.New("root genesis difficulty must be a positive uint256")
	}
	if (*big.Int)(g.Difficulty).Cmp(g.PoSW.MinimumDifficulty) < 0 {
		return errors.New("root genesis difficulty is below MinimumDifficulty")
	}
	return nil
}

// prepareShard fills QKC fields and contract code in a freshly decoded definition.
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
	if g.Mixhash.Big().Cmp(minimum) < 0 {
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
	return g.ValidatePoSWStakingContracts()
}

// decodeJSON accepts one document, preserving native codecs and rejecting unknown
// fields in plain structs such as RootGenesis.
func decodeJSON(file string, target any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%s: expected a single JSON document", file)
	}
	return nil
}
