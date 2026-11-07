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
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/shard"
)

// bootstrapEngine keeps native header, execution and import validation while
// allowing the test to import an unsealed block built from the generated G0.
type bootstrapEngine struct{ *posw.PoSW }

func (bootstrapEngine) VerifySeal(consensus.ChainHeaderReader, *types.Header) error {
	return nil
}

type bootstrapBackend struct {
	chain *core.BlockChain
	pool  *txpool.TxPool
}

func (b bootstrapBackend) BlockChain() *core.BlockChain { return b.chain }
func (b bootstrapBackend) TxPool() *txpool.TxPool       { return b.pool }

func TestGenesisBuildAndImportFirstBlock(t *testing.T) {
	for _, test := range []struct {
		name     string
		advanced bool
	}{
		{"London", false},
		{"Amsterdam", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster := exampleInputs(t)
			for _, slave := range cluster.Slaves {
				g := readShard(t, slave.GenesisFile)
				g.ExtraData = nil
				delete(g.Alloc, params.ShardStakingContractAddress)
				delete(g.Alloc, params.RootStakingContractAddress)
				if test.advanced {
					cfg := g.Config
					cfg.ShanghaiTime, cfg.CancunTime, cfg.PragueTime, cfg.OsakaTime, cfg.AmsterdamTime = new(uint64), new(uint64), new(uint64), new(uint64), new(uint64)
					cfg.BlobScheduleConfig = &params.BlobScheduleConfig{Cancun: params.DefaultCancunBlobConfig, Prague: params.DefaultPragueBlobConfig}
				}
				writeJSON(t, slave.GenesisFile, g)
			}
			bundle, err := Load(cluster)
			if err != nil {
				t.Fatal(err)
			}
			for _, generated := range bundle.Shards {
				t.Run(generated.Genesis.Config.ChainID.String(), func(t *testing.T) {
					db := rawdb.NewMemoryDatabase()
					t.Cleanup(func() { db.Close() })
					pow, err := posw.New(generated.Genesis.Config, generated.Block.Header())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { pow.Close() })
					engine := bootstrapEngine{pow}
					chain, err := core.NewBlockChain(db, generated.Genesis, engine, nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(chain.Stop)
					poolConfig := legacypool.DefaultConfig
					poolConfig.Journal = ""
					pool, err := txpool.New(poolConfig.PriceLimit, chain, []txpool.SubPool{legacypool.New(poolConfig, chain)})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := pool.Close(); err != nil {
							t.Errorf("close transaction pool: %v", err)
						}
					})
					minerConfig := miner.DefaultConfig
					minerConfig.GasCeil = generated.Block.GasLimit()
					builder := miner.New(bootstrapBackend{chain, pool}, minerConfig, engine)
					sc := shard.NewShardChain(db, chain, builder)
					if err := sc.Init(bundle.Root); err != nil {
						t.Fatal(err)
					}
					// No remote G0 deposit lists are seeded. A usable genesis cursor
					// must skip R0 so block 1 can build and pass full import checks.
					block, err := sc.BuildBlock(context.Background(), miner.QKCBlockArgs{
						Timestamp:    generated.Block.Time() + 1,
						FeeRecipient: common.Address{0xcc},
						NoTxs:        true,
					})
					if err != nil {
						t.Fatalf("build first shard block: %v", err)
					}
					if block.NumberU64() != 1 || block.ParentHash() != generated.Block.Hash() {
						t.Fatal("first block does not extend the generated shard genesis")
					}
					if err := sc.InsertShardBlock(block); err != nil {
						t.Fatalf("import first shard block: %v", err)
					}
					if chain.CurrentBlock().Hash() != block.Hash() {
						t.Fatal("imported first block did not become the canonical shard head")
					}
				})
			}
		})
	}
}
