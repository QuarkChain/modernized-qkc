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
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/crypto"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

// verifySeal selects the effective staking difficulty, then checks Ethash outside
// simulation. Header fields must already pass verifyHeader.
func (p *PoSW) verifySeal(chain consensus.RootChainReader, header, parent *qkctypes.RootBlockHeader) error {
	difficulty, err := p.adjustedDifficulty(chain, header, parent)
	if err != nil {
		return err
	}
	// Simulation keeps staking reads and their errors, and skips only Ethash.
	if p.simulate {
		return nil
	}
	// Outside simulation, missing authorization never skips PoW.
	return p.EthashVerifier.Verify(uint64(header.Number), header.SealHash(), header.Nonce, header.MixDigest, difficulty)
}

// adjustedDifficulty checks parent-confirmed stakes, the authorized signer and
// this coinbase's quota on the parent branch. It leaves Difficulty and TD unchanged.
func (p *PoSW) adjustedDifficulty(chain consensus.RootChainReader, header, parent *qkctypes.RootBlockHeader) (*big.Int, error) {
	base := header.Difficulty.ToBig()
	// Disabled staking needs neither staking state nor a history walk.
	if !p.config.PoSW.StakingEnabled {
		return base, nil
	}
	// Pin stakes to the candidate parent's last confirmation on the configured
	// root staking chain. Neither the current head nor candidate body is used.
	chainID := p.config.ChainIDs[0]
	lastConfirmed, err := chain.GetLastConfirmedShardBlocks(parent.Hash())
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
	// Invalid or missing authorization removes the discount, not the PoW check.
	if signer == (common.Address{}) {
		return base, nil
	}
	// Require V=0/1 so CGO and pure Go recovery apply the same consensus rule.
	if header.Signature[64] > 1 {
		return base, nil
	}
	// Recover the candidate SealHash signer and match the authorized address
	// read from the confirmed staking state.
	pubkey, err := crypto.SigToPub(header.SealHash().Bytes(), header.Signature[:])
	if err != nil || crypto.PubkeyToAddress(*pubkey) != signer {
		return base, nil
	}
	// Count all roots by this coinbase in the preceding WindowSize-1 ancestors,
	// including ordinary PoW blocks and genesis when in range. The candidate
	// occupies the last window slot.
	var mined uint64
	for i, ancestor := uint64(0), parent; i < p.config.PoSW.WindowSize-1; i++ {
		if ancestor.Coinbase == header.Coinbase {
			mined++
		}
		if ancestor.Number == 0 || i+1 == p.config.PoSW.WindowSize-1 {
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
	// Apply the shared stake quota and divider using the candidate's height.
	return posw.CalcAdjustedDifficulty(p.config.PoSW, new(big.Int).SetUint64(uint64(header.Number)), base, stakes, mined), nil
}
