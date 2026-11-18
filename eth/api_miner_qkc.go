// Copyright 2026 The go-ethereum Authors
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

package eth

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/shard"
)

// qkcMiningAPI serves external miners on QuarkChain shards.
type qkcMiningAPI struct {
	e *Ethereum
}

// GetWork returns a QKC candidate for an external miner. Difficulty is the
// effective PoSW difficulty; Number selects the Ethash epoch.
func (api *qkcMiningAPI) GetWork(ctx context.Context, coinbase common.Address) (*shard.MiningWork, error) {
	chainID := api.e.BlockChain().Config().ChainID
	work, err := api.e.PoWMiner().GetWork(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	log.Info("Served shard mining work", "chain", chainID, "coinbase", coinbase, "number", work.Number, "sealhash", work.SealHash, "difficulty", work.Difficulty)
	return work, nil
}

// SubmitWork verifies and imports a sealed QKC candidate. Invalid seals return
// an error and leave the work available for another submission.
func (api *qkcMiningAPI) SubmitWork(sealHash common.Hash, nonce types.BlockNonce, mixHash common.Hash) (bool, error) {
	chainID := api.e.BlockChain().Config().ChainID
	if err := api.e.PoWMiner().SubmitWork(sealHash, nonce, mixHash); err != nil {
		return false, err
	}
	log.Info("Accepted shard mining work", "chain", chainID, "sealhash", sealHash, "nonce", nonce.Uint64())
	return true, nil
}
