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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	posw "github.com/ethereum/go-ethereum/consensus/poswshard"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// PoSWInfo describes a root coinbase's staking quota, usage and effective difficulty.
type PoSWInfo struct {
	EffectiveDifficulty *big.Int       // Base difficulty unless the signature and quota allow a discount.
	Signer              common.Address // Authorized signer from parent-confirmed staking state.
	Quota               uint64         // min(WindowSize, stakes / StakePerBlockAt(number)).
	Mined               uint64         // This coinbase's blocks in the preceding WindowSize-1, plus the candidate.
}

// verifySeal selects the effective staking difficulty, then checks Ethash outside
// simulation. Header fields must already pass verifyHeader.
func (p *PoSW) verifySeal(chain RootChainReader, header *qkctypes.RootBlockHeader) error {
	info, err := GetPoSWInfo(p.config, chain, header)
	if err != nil {
		return err
	}
	difficulty := header.Difficulty.ToBig()
	if info != nil {
		difficulty = info.EffectiveDifficulty
	}
	// Simulation keeps staking reads and their errors, and skips only Ethash.
	if p.simulate {
		return nil
	}
	// Outside simulation, missing authorization never skips PoW.
	return p.EthashVerifier.Verify(uint64(header.Number), header.SealHash(), header.Nonce, header.MixDigest, difficulty)
}

// GetPoSWInfo reads parent-confirmed stakes and counts this coinbase's blocks on
// the parent branch, then checks the signature to select the effective difficulty.
// Invalid signatures keep the base difficulty without discarding quota or counts.
// The header and stored chain data are unchanged.
// config must pass Check; the header must be valid apart from its unverified seal.
// Staking disabled or genesis returns nil without reading chain. Mined includes
// the candidate as if it were added, even when quota is zero or the signature fails.
func GetPoSWInfo(config *params.QuarkChainConfig, chain RootChainReader, header *qkctypes.RootBlockHeader) (*PoSWInfo, error) {
	if !config.PoSW.StakingEnabled || header.Number == 0 {
		return nil, nil
	}
	// Pin stakes to the candidate parent's last confirmation on the configured
	// root staking chain. Neither the current head nor candidate body is used.
	chainID := config.ChainIDs[0]
	lastConfirmed, err := chain.GetLastConfirmedShardBlocks(header.ParentHash)
	if err != nil {
		return nil, err
	}
	confirmed, ok := lastConfirmed[chainID.String()]
	// All shards start at root genesis; a missing confirmation is a data error,
	// rather than a reason to fall back to ordinary PoW as pyquarkchain does.
	if !ok || confirmed == (common.Hash{}) {
		return nil, errors.New("missing parent root staking shard confirmation")
	}
	stakes, signer, err := chain.GetRootChainStakes(chainID, confirmed, header.Coinbase)
	if err != nil {
		return nil, err
	}
	// Zero stake is a non-nil zero value; nil means required data is unavailable.
	if stakes == nil {
		return nil, errors.New("missing root staking result")
	}
	info := &PoSWInfo{
		EffectiveDifficulty: header.Difficulty.ToBig(),
		Signer:              signer,
		Quota:               posw.StakeQuota(config.PoSW, new(big.Int).SetUint64(uint64(header.Number)), stakes),
		Mined:               1,
	}
	if config.PoSW.WindowSize > 1 {
		parent, err := chain.GetRootHeader(header.ParentHash)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, consensus.ErrUnknownAncestor
		}
		if parent.Number+1 != header.Number || parent.Hash() != header.ParentHash {
			return nil, errors.New("inconsistent root staking history")
		}
		// Add this coinbase's roots in the preceding WindowSize-1 ancestors,
		// including genesis when in range. Mined already includes the candidate.
		for i, ancestor := uint64(0), parent; i < config.PoSW.WindowSize-1; i++ {
			if ancestor.Coinbase == header.Coinbase {
				info.Mined++
			}
			if ancestor.Number == 0 || i+1 == config.PoSW.WindowSize-1 {
				break
			}
			previous, err := chain.GetRootHeader(ancestor.ParentHash)
			if err != nil {
				return nil, err
			}
			if previous == nil {
				return nil, consensus.ErrUnknownAncestor
			}
			// Keep the count on this candidate's branch, with no gaps in its history.
			if previous.Number+1 != ancestor.Number || previous.Hash() != ancestor.ParentHash {
				return nil, errors.New("inconsistent root staking history")
			}
			ancestor = previous
		}
	}
	// Mined includes the candidate, so it may equal the quota and still qualify.
	if info.Mined > info.Quota || signer == (common.Address{}) {
		return info, nil
	}
	// Require V=0/1 so CGO and pure Go recovery apply the same consensus rule.
	if header.Signature[64] > 1 {
		return info, nil
	}
	pubkey, err := crypto.SigToPub(header.SealHash().Bytes(), header.Signature[:])
	if err != nil || crypto.PubkeyToAddress(*pubkey) != signer {
		return info, nil
	}
	info.EffectiveDifficulty = posw.DiscountDifficulty(config.PoSW.DiffDivider, info.EffectiveDifficulty)
	return info, nil
}
