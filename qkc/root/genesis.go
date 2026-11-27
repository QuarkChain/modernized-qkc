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

package root

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
)

// SetupRootGenesisBlock writes or verifies the root genesis block (R0).
// The shards argument is required for both initialization and restart, even
// when seed is nil. Supply one genesis per configured chain, in ChainIDs order.
// On restart with a nil seed, setup rebuilds the shard genesis headers using
// the stored root rules and R0 timestamp and checks their commitment against R0.
// Call with exclusive access to the database before starting the chain.
func SetupRootGenesisBlock(db ethdb.Database, seed *RootGenesis, shards []*core.Genesis) (*params.QuarkChainConfig, common.Hash, error) {
	if db == nil {
		return nil, common.Hash{}, errors.New("missing root database")
	}
	hash := rawdb.ReadRootCanonicalHash(db, 0)
	if hash == (common.Hash{}) && seed == nil {
		return nil, common.Hash{}, errors.New("root genesis is required to initialize the database")
	}
	var genesis *types.RootBlock
	if seed != nil {
		var err error
		genesis, err = BuildRootGenesis(seed, shards)
		if err != nil {
			return nil, common.Hash{}, err
		}
		supplied := genesis.Hash()
		if hash == (common.Hash{}) {
			// First startup writes the genesis records in one batch.
			// An existing head makes a missing height-zero index an error.
			if err := commitRootGenesis(db, genesis, seed.Config); err != nil {
				return nil, common.Hash{}, err
			}
			return seed.Config, supplied, nil
		}
		if supplied != hash {
			return nil, hash, &core.GenesisMismatchError{Stored: hash, New: supplied}
		}
	}
	r0, stored, err := readStoredRootGenesis(db, hash)
	if err != nil {
		return nil, hash, err
	}
	if seed != nil {
		// With a supplied seed, a matching R0 hash still requires matching
		// consensus rules and tracking data.
		if !stored.Equal(seed.Config) {
			return nil, hash, errors.New("root consensus configuration differs from the stored configuration")
		}
		if !bytes.Equal(genesis.TrackingData(), r0.TrackingData()) {
			return nil, hash, errors.New("root genesis tracking data differs from stored block")
		}
	} else {
		// For a nil seed, rebuild the shard headers under the stored rules
		// and compare their commitment with R0.
		headers, err := buildShardGenesisHeaders(stored, r0.Time(), shards)
		if err != nil {
			return nil, hash, err
		}
		if types.CalcShardHeaderHash(headers) != r0.ShardHeaderHash() {
			return nil, hash, errors.New("configured shard genesis headers differ from stored root genesis")
		}
	}
	return stored, hash, nil
}

func readStoredRootGenesis(db ethdb.KeyValueReader, hash common.Hash) (*types.RootBlock, *params.QuarkChainConfig, error) {
	config, err := rawdb.ReadRootChainConfig(db)
	if err != nil {
		return nil, nil, fmt.Errorf("read root consensus configuration: %w", err)
	}
	if err := checkRootConsensusConfig(config); err != nil {
		return nil, nil, fmt.Errorf("stored root consensus configuration: %w", err)
	}
	r0 := rawdb.ReadRootBlock(db, hash)
	if r0 == nil || r0.Hash() != hash {
		return nil, nil, fmt.Errorf("missing or inconsistent root genesis block %x", hash)
	}
	confirmed, err := genesisConfirmations(r0, config)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := rawdb.ReadRootConfirmedShardBlocks(db, hash)
	if err != nil {
		return nil, nil, fmt.Errorf("read root genesis confirmations: %w", err)
	}
	if !maps.Equal(confirmed, snapshot) {
		return nil, nil, errors.New("stored root genesis confirmations differ from the genesis shard headers")
	}
	return r0, config, nil
}

func commitRootGenesis(db ethdb.Database, genesis *types.RootBlock, config *params.QuarkChainConfig) error {
	hasHead, err := rawdb.HasRootHeadBlockHash(db)
	if err != nil {
		return fmt.Errorf("check for existing root head: %w", err)
	}
	if hasHead {
		return errors.New("root database has a head but no canonical genesis")
	}
	confirmed, err := genesisConfirmations(genesis, config)
	if err != nil {
		return err
	}
	batch := db.NewBatch()
	defer batch.Close()
	hash := genesis.Hash()
	if err := rawdb.WriteRootBlockWithError(batch, genesis); err != nil {
		return err
	}
	if err := rawdb.WriteRootCanonicalHash(batch, hash, 0); err != nil {
		return err
	}
	if err := rawdb.WriteRootHeadBlockHash(batch, hash); err != nil {
		return err
	}
	if err := rawdb.WriteRootConfirmedShardBlocks(batch, hash, confirmed); err != nil {
		return err
	}
	if err := rawdb.WriteRootChainConfig(batch, config); err != nil {
		return err
	}
	return batch.Write()
}

// genesisConfirmations derives the initial snapshot from exactly one G0 per
// configured member. R0 commits shard headers in ascending numeric chain ID order.
func genesisConfirmations(genesis *types.RootBlock, config *params.QuarkChainConfig) (map[string]common.Hash, error) {
	h := genesis.Header()
	if err := h.SanityCheck(); err != nil {
		return nil, err
	}
	if h.Number != 0 || h.ParentHash != (common.Hash{}) || !h.TotalDifficulty.Eq(&h.Difficulty) {
		return nil, errors.New("root genesis requires height zero, zero parent and total difficulty equal to difficulty")
	}
	if !h.CoinbaseAmount.IsZero() {
		return nil, errors.New("root genesis cannot pay a block reward")
	}
	if h.Difficulty.ToBig().Cmp(config.PoSW.MinimumDifficulty) < 0 {
		return nil, errors.New("root genesis difficulty is below the configured minimum")
	}
	headers := genesis.ShardBlockHeaders()
	if len(headers) != len(config.ChainIDs) {
		return nil, fmt.Errorf("root genesis confirms %d shards, want %d", len(headers), len(config.ChainIDs))
	}
	chainIDs := slices.Clone(config.ChainIDs)
	slices.SortFunc(chainIDs, (*big.Int).Cmp)
	confirmed := make(map[string]common.Hash, len(headers))
	for i, g0 := range headers {
		id := chainIDs[i]
		if g0 == nil || g0.Number == nil || g0.Number.Sign() != 0 || g0.ParentHash != (common.Hash{}) {
			return nil, fmt.Errorf("expected shard genesis header for chain %s at position %d", id, i)
		}
		if err := g0.SanityCheckQKC(); err != nil {
			return nil, fmt.Errorf("shard genesis header for chain %s: %w", id, err)
		}
		extra, _ := g0.QKCExtra() // SanityCheckQKC validated the encoded fields.
		if err := extra.CheckGenesis(id, g0.GasLimit); err != nil {
			return nil, fmt.Errorf("shard genesis header for chain %s: %w", id, err)
		}
		if g0.GasUsed != 0 || g0.MixDigest == (common.Hash{}) || !g0.EmptyBody() || !g0.EmptyReceipts() {
			return nil, fmt.Errorf("invalid shard genesis gas, difficulty, body or receipts for chain %s", id)
		}
		confirmed[id.String()] = g0.Hash()
	}
	if types.CalcShardHeaderHash(headers) != h.ShardHeaderHash {
		return nil, errors.New("root genesis shard header commitment mismatch")
	}
	return confirmed, nil
}
