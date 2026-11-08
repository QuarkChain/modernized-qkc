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
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

func staking(config *params.QuarkChainConfig) { config.PoSW.StakingEnabled = true }

// authorize puts stakes and a signer in the parent root's confirmed shard state,
// then signs the candidate's SealHash with that signer's fixed test key.
func authorize(t *testing.T, f *fixture, block *qkctypes.RootBlock, stakes uint64) *qkctypes.RootBlock {
	t.Helper()
	key, err := crypto.ToECDSA(common.LeftPadBytes([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	id := f.engine.config.ChainIDs[0]
	confirmed := f.chain.confirmed[block.ParentHash()][id.String()]
	f.chain.stakes[confirmed.Hash()] = stakeResult{uint256.NewInt(stakes), crypto.PubkeyToAddress(key.PublicKey)}
	header := block.Header()
	if err := header.Sign(key); err != nil {
		t.Fatal(err)
	}
	return block.WithSeal(header)
}

// Check difficulty selection directly: only the authorized signer with enough
// stakes gets a discount. Base difficulty and total difficulty stay unchanged.
func TestSignerAndQuota(t *testing.T) {
	for _, name := range []string{"authorized", "zero stakes", "wrong signer", "zero signer", "missing signature", "bad signature", "invalid recovery ID"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, staking)
			block := authorize(t, f, f.candidate(t), 100)
			confirmed := f.chain.confirmed[f.parent.Hash()]["10001"]
			result := f.chain.stakes[confirmed.Hash()]
			switch name {
			case "zero stakes":
				result.stakes = new(uint256.Int)
			case "wrong signer":
				result.signer = common.Address{7}
			case "zero signer":
				result.signer = common.Address{}
			case "missing signature":
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature = [65]byte{} })
			case "bad signature":
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature[64] = 255 })
			case "invalid recovery ID":
				// Pure Go recovery accepts V+4 as the same signer; root consensus must not.
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature[64] += 4 })
			}
			f.chain.stakes[confirmed.Hash()] = result
			before := block.Header()
			difficulty, err := f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
			if err != nil {
				t.Fatal(err)
			}
			want := int64(100)
			if name == "authorized" {
				want = 10
			}
			if difficulty.Int64() != want || f.chain.stakeHash != confirmed.Hash() || f.chain.stakeCoinbase != block.Coinbase() {
				t.Fatal("wrong stake source or effective difficulty")
			}
			if block.Difficulty().Cmp(&before.Difficulty) != 0 || block.TotalDifficulty().Cmp(&before.TotalDifficulty) != 0 {
				t.Fatal("staking changed base difficulty or total difficulty")
			}
		})
	}
}

// A proof valid only at the discounted target checks that VerifyBlock passes
// the adjusted difficulty to Ethash and still checks PoW after authorization.
func TestDiscountedEthashSeal(t *testing.T) {
	for _, name := range []string{"authorized", "zero stakes", "wrong signer", "zero signer", "missing signature", "bad signature", "invalid recovery ID", "disabled", "missing confirmation"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *params.QuarkChainConfig) { c.PoSW.StakingEnabled = name != "disabled" })
			block := authorize(t, f, f.candidate(t), 100)
			confirmed := f.chain.confirmed[f.parent.Hash()]["10001"]
			result := f.chain.stakes[confirmed.Hash()]
			switch name {
			case "zero stakes":
				result.stakes = new(uint256.Int)
			case "wrong signer":
				result.signer = common.Address{7}
			case "zero signer":
				result.signer = common.Address{}
			case "missing signature":
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature = [65]byte{} })
			case "bad signature":
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature[64] = 255 })
			case "invalid recovery ID":
				block = changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.Signature[64] += 4 })
			case "missing confirmation":
				delete(f.chain.confirmed[f.parent.Hash()], "10001")
			}
			f.chain.stakes[confirmed.Hash()] = result
			// This fixed proof meets difficulty 10 but fails difficulty 100.
			// All cases share the seal hash from TestRealEthashIsChecked.
			block = block.WithMiningResult(5, common.HexToHash("0xfe3841ae5cc7b3068f24e6ccf2109e313657958a82f7e3090434d297ce20d82e"), nil)
			err := f.engine.VerifyBlock(f.chain, block)
			if name == "authorized" {
				if err != nil {
					t.Fatalf("discounted proof rejected: %v", err)
				}
				invalid := changeHeader(block, func(h *qkctypes.RootBlockHeader) { h.MixDigest[0] ^= 1 })
				if err := f.engine.VerifyBlock(f.chain, invalid); err == nil || !strings.Contains(err.Error(), "mix hash") {
					t.Fatalf("authorized signer bypassed PoW verification: %v", err)
				}
			} else if name == "missing confirmation" {
				if err == nil || !strings.Contains(err.Error(), "missing parent root staking shard confirmation") {
					t.Fatalf("missing confirmation error lost: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "proof of work") {
				t.Fatalf("base-difficulty proof check missing: %v", err)
			}
		})
	}
}

// appendRoot creates synthetic branch history without mining. It carries forward
// the parent's shard confirmations so window tests can control only root authors.
func (f *fixture) appendRoot(t *testing.T, coinbase common.Address) *qkctypes.RootBlock {
	t.Helper()
	block := changeHeader(f.candidate(t), func(h *qkctypes.RootBlockHeader) { h.Coinbase = coinbase })
	confirmations := make(map[string]*types.Header)
	for id, header := range f.chain.confirmed[f.parent.Hash()] {
		confirmations[id] = header
	}
	f.chain.blocks[block.Hash()], f.chain.confirmed[block.Hash()] = block, confirmations
	f.parent = block
	return block
}

// Count the coinbase's blocks only in the preceding windowSize-1 ancestors.
// Genesis counts inside that window; a window of one has no prior blocks.
func TestQuotaUsesParentBranchWindow(t *testing.T) {
	for _, name := range []string{"below quota", "quota used", "outside window", "genesis counted", "window one"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *params.QuarkChainConfig) {
				staking(c)
				if name == "window one" {
					c.PoSW.WindowSize = 1
				}
			})
			coinbase := common.Address{9}
			if name == "genesis counted" {
				parent := changeHeader(f.parent, func(h *qkctypes.RootBlockHeader) { h.Coinbase = coinbase })
				f.chain.blocks[parent.Hash()], f.chain.confirmed[parent.Hash()] = parent, f.chain.confirmed[f.parent.Hash()]
				f.parent = parent
			} else if name != "window one" {
				f.appendRoot(t, coinbase)
				if name == "quota used" {
					f.appendRoot(t, coinbase)
				} else {
					f.appendRoot(t, common.Address{})
				}
				f.appendRoot(t, common.Address{})
				if name == "outside window" {
					f.appendRoot(t, common.Address{})
				}
			}
			stakes := uint64(200)
			if name == "genesis counted" || name == "outside window" {
				stakes = 100
			}
			block := authorize(t, f, f.candidate(t), stakes)
			difficulty, err := f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
			if err != nil {
				t.Fatal(err)
			}
			want := int64(10)
			if name == "quota used" || name == "genesis counted" {
				want = 100
			}
			if difficulty.Int64() != want {
				t.Fatalf("difficulty %v, want %d", difficulty, want)
			}
		})
	}
}

// Stakes come from the candidate parent's confirmation on its own branch,
// excluding both newly included shard headers and another root branch's state.
func TestStakesUseConfirmedStateBeforeCandidate(t *testing.T) {
	f := newFixture(t, staking)
	newShard := f.shard(10001, 3)
	block := authorize(t, f, f.candidate(t, newShard), 0)
	// The new confirmation funds the same coinbase, but it cannot give this
	// candidate a discount. Eligibility comes from its parent's confirmation.
	f.chain.stakes[newShard.Hash()] = stakeResult{uint256.NewInt(10000), common.Address{1}}
	difficulty, err := f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
	if err != nil {
		t.Fatal(err)
	}
	oldShard := f.chain.confirmed[f.parent.Hash()]["10001"]
	if f.chain.stakeHash != oldShard.Hash() || difficulty.Int64() != 100 {
		t.Fatal("used candidate confirmation state")
	}

	// A side root branch has a different last confirmation. The reader also
	// retains the other branch, but only the candidate parent's record is used.
	mainParent := f.appendRoot(t, common.Address{})
	mainConfirmed := f.chain.confirmed[mainParent.Hash()]
	forkShard := f.extendShard(oldShard, mainParent.ParentHash(), 0)
	forkShard.Time = mainParent.Time()
	f.chain.shardAmounts[forkShard.Hash()] = new(uint256.Int)
	fork := changeHeader(mainParent, func(h *qkctypes.RootBlockHeader) { h.Extra = []byte("side branch") })
	fork = qkctypes.NewRootBlock(fork.Header(), qkctypes.ShardBlockHeaders{forkShard}, nil)
	forkConfirmed := make(map[string]*types.Header)
	for id, header := range mainConfirmed {
		forkConfirmed[id] = header
	}
	forkConfirmed["10001"] = forkShard
	f.chain.blocks[fork.Hash()], f.chain.confirmed[fork.Hash()] = fork, forkConfirmed
	f.parent = fork
	block = authorize(t, f, f.candidate(t), 100)
	difficulty, err = f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
	if err != nil {
		t.Fatal(err)
	}
	if f.chain.stakeHash != forkShard.Hash() || difficulty.Int64() != 10 {
		t.Fatal("used another branch's stakes")
	}
}

// Disabled staking uses base difficulty without reading confirmation or stakes.
// Missing required data and read failures must return errors.
func TestStakingFallbackAndReadFailures(t *testing.T) {
	failure := errors.New("staking read failed")
	for _, name := range []string{"disabled", "missing confirmation", "confirmation error", "stakes error", "nil stakes", "missing ancestor", "ancestor error"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(c *params.QuarkChainConfig) {
				c.PoSW.StakingEnabled = name != "disabled"
			})
			var missing common.Hash
			if name == "missing ancestor" || name == "ancestor error" {
				missing = f.appendRoot(t, common.Address{}).Hash()
				f.appendRoot(t, common.Address{})
			}
			block := authorize(t, f, f.candidate(t), 100)
			confirmed := f.chain.confirmed[f.parent.Hash()]["10001"]
			want := failure
			switch name {
			case "disabled":
				f.chain.confirmationErr = failure
				want = nil
			case "missing confirmation":
				delete(f.chain.confirmed[f.parent.Hash()], "10001")
				f.chain.stakeErr = failure
			case "confirmation error":
				f.chain.confirmationErr = failure
			case "stakes error":
				f.chain.stakeErr = failure
			case "nil stakes":
				f.chain.stakes[confirmed.Hash()] = stakeResult{}
				want = nil
			case "missing ancestor":
				delete(f.chain.blocks, missing)
				want = consensus.ErrUnknownAncestor
			case "ancestor error":
				f.chain.headerErr = failure
			}
			difficulty, err := f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
			if name == "missing confirmation" {
				if err == nil || !strings.Contains(err.Error(), "missing parent root staking shard confirmation") {
					t.Fatalf("missing confirmation did not fail before reading stakes: %v", err)
				}
			} else if name == "nil stakes" {
				if err == nil {
					t.Fatal("nil staking result accepted")
				}
			} else if !errors.Is(err, want) {
				t.Fatalf("error %v, want %v", err, want)
			}
			if name == "disabled" && difficulty.Int64() != 100 {
				t.Fatal("fallback did not use base difficulty")
			}
		})
	}
}

// The staking chain is the first configured ID, even when it is not the smallest.
// IDs must retain all bits rather than being treated as a small shard index.
func TestConfiguredStakingChainAndLargeIDs(t *testing.T) {
	large := new(big.Int).Lsh(big.NewInt(1), 128)
	f := newFixture(t, func(c *params.QuarkChainConfig) {
		staking(c)
		c.ChainIDs = []*big.Int{large, big.NewInt(10001)}
		c.PoSW.OriginalBlockNumber = 50000
	})
	first := f.shard(10001, 1)
	second := f.extendShard(f.chain.confirmed[f.parent.Hash()][large.String()], f.parent.Hash(), 2)
	block := authorize(t, f, f.candidate(t, first, second), 100)
	if err := f.verifyUnsealed(block); err != nil {
		t.Fatalf("body with large chain ID rejected: %v", err)
	}
	difficulty, err := f.engine.adjustedDifficulty(f.chain, block.Header(), f.parent.Header())
	if err != nil {
		t.Fatal(err)
	}
	if f.chain.stakeChain.Cmp(large) != 0 || difficulty.Int64() != 10 {
		t.Fatal("staking chain used a fixed or shortened chain ID")
	}
}
