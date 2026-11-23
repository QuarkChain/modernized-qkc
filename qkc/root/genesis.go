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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/types"
)

// InitializeRootDatabase installs or verifies the genesis described by a loaded root TOML.
// When the root seed is omitted, the stored rules and R0 must still agree with the configured shards.
func InitializeRootDatabase(db ethdb.Database, cfg *config.RootConfig) (*params.QuarkChainConfig, common.Hash, error) {
	if cfg == nil {
		return nil, common.Hash{}, errors.New("missing root configuration")
	}
	if cfg.Genesis != nil {
		genesis, err := BuildRootGenesis(cfg)
		if err != nil {
			return nil, common.Hash{}, err
		}
		return SetupRootGenesisBlock(db, genesis, cfg.Genesis.Config)
	}
	rules, hash, err := SetupRootGenesisBlock(db, nil, nil)
	if err != nil {
		return nil, hash, err
	}
	header := rawdb.ReadRootBlockHeader(db, hash)
	if header == nil {
		return nil, hash, fmt.Errorf("cannot read stored root genesis %s", hash)
	}
	// Reuse the loaded-config validation and G0 construction with the stored
	// root seed, keeping the caller's omitted genesis unchanged.
	restored := *cfg
	restored.Genesis = &config.RootGenesis{
		Timestamp: header.Time, Difficulty: header.Difficulty.ToBig(),
		Coinbase: header.Coinbase, Config: rules,
	}
	genesis, err := BuildRootGenesis(&restored)
	if err != nil {
		return nil, hash, err
	}
	if header.ShardHeaderHash != genesis.ShardHeaderHash() {
		return nil, hash, errors.New("configured shard genesis headers differ from stored root genesis")
	}
	return rules, hash, nil
}

// SetupRootGenesisBlock writes genesis in one batch when canonical(0) is absent,
// or checks the existing genesis and consensus rules without rewinding the head.
// An initialized database can supply both values when genesis and config are
// nil. A supplied genesis requires its config. Call with exclusive access to
// the database before starting the chain.
func SetupRootGenesisBlock(db ethdb.Database, genesis *types.RootBlock, config *params.QuarkChainConfig) (*params.QuarkChainConfig, common.Hash, error) {
	if db == nil {
		return nil, common.Hash{}, errors.New("missing root database")
	}
	var want map[string]common.Hash
	if config != nil || genesis != nil {
		if err := config.Check(); err != nil {
			return nil, common.Hash{}, fmt.Errorf("root consensus configuration: %w", err)
		}
		if genesis != nil {
			var err error
			want, err = genesisConfirmations(genesis, config)
			if err != nil {
				return nil, common.Hash{}, err
			}
		}
	}
	hash, err := rawdb.ReadRootGenesisHash(db)
	if err != nil {
		return nil, common.Hash{}, fmt.Errorf("read canonical root genesis: %w", err)
	}
	if hash == (common.Hash{}) {
		if genesis == nil {
			return nil, common.Hash{}, errors.New("root genesis is required to initialize the database")
		}
		if err := commitRootGenesis(db, genesis, config, want); err != nil {
			return nil, common.Hash{}, err
		}
		return config, genesis.Hash(), nil
	}
	if genesis != nil && genesis.Hash() != hash {
		return nil, hash, fmt.Errorf("root genesis mismatch: stored %x, supplied %x", hash, genesis.Hash())
	}
	stored, err := rawdb.ReadRootChainConfig(db)
	if err != nil {
		return nil, hash, fmt.Errorf("read root consensus configuration: %w", err)
	}
	if err := stored.Check(); err != nil {
		return nil, hash, fmt.Errorf("stored root consensus configuration: %w", err)
	}
	if config != nil && !stored.Equal(config) {
		return nil, hash, errors.New("root consensus configuration differs from the stored configuration")
	}
	tip, err := readRootHead(db, stored)
	if err != nil {
		return nil, hash, err
	}
	r0 := tip
	if tip.Hash() != hash {
		r0 = rawdb.ReadRootBlock(db, hash)
		if r0 == nil || r0.Hash() != hash {
			return nil, hash, fmt.Errorf("missing or inconsistent root genesis block %x", hash)
		}
	}
	confirmed, err := genesisConfirmations(r0, stored)
	if err != nil {
		return nil, hash, err
	}
	if genesis != nil && !bytes.Equal(genesis.TrackingData(), r0.TrackingData()) {
		return nil, hash, errors.New("root genesis tracking data differs from stored block")
	}
	snapshot, err := rawdb.ReadRootConfirmedShardBlocks(db, hash)
	if err != nil {
		return nil, hash, fmt.Errorf("read root genesis confirmations: %w", err)
	}
	if !maps.Equal(confirmed, snapshot) {
		return nil, hash, errors.New("stored root genesis confirmations differ from the genesis shard headers")
	}
	return stored, hash, nil
}

func commitRootGenesis(db ethdb.Database, genesis *types.RootBlock, config *params.QuarkChainConfig, confirmed map[string]common.Hash) error {
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
	if err := batch.Write(); err != nil {
		return err
	}
	return db.SyncKeyValue()
}

// genesisConfirmations derives the initial snapshot from exactly one G0 per
// configured member. ChainIDs order determines both the root reward shard and
// the order of headers committed by R0.
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
	confirmed := make(map[string]common.Hash, len(headers))
	for i, g0 := range headers {
		id := config.ChainIDs[i]
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
