// Copyright 2023 The go-ethereum Authors
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
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/shard"
)

// MinerAPI provides an API to control the miner.
type MinerAPI struct {
	e *Ethereum
}

// NewMinerAPI creates a new MinerAPI instance.
func NewMinerAPI(e *Ethereum) *MinerAPI {
	return &MinerAPI{e}
}

// GetWork returns a QKC candidate for an external miner. Difficulty is the
// effective PoSW difficulty; Number selects the Ethash epoch.
func (api *MinerAPI) GetWork(ctx context.Context, coinbase common.Address) (*shard.MiningWork, error) {
	if api.e.PoWMiner() == nil {
		return nil, errors.New("mining work is only available on QuarkChain shards")
	}
	return api.e.PoWMiner().GetWork(ctx, coinbase)
}

// SubmitWork verifies and imports a sealed QKC candidate. Invalid seals return
// an error and leave the work available for another submission.
func (api *MinerAPI) SubmitWork(sealHash common.Hash, nonce types.BlockNonce, mixHash common.Hash) (bool, error) {
	if api.e.PoWMiner() == nil {
		return false, errors.New("mining work is only available on QuarkChain shards")
	}
	if err := api.e.PoWMiner().SubmitWork(sealHash, nonce, mixHash); err != nil {
		return false, err
	}
	return true, nil
}

// SetExtra sets the extra data string that is included when this miner mines a block.
func (api *MinerAPI) SetExtra(extra string) (bool, error) {
	if err := api.e.Miner().SetExtra([]byte(extra)); err != nil {
		return false, err
	}
	return true, nil
}

// SetGasPrice sets the minimum accepted gas price for the miner.
func (api *MinerAPI) SetGasPrice(gasPrice hexutil.Big) bool {
	api.e.lock.Lock()
	api.e.gasPrice = (*big.Int)(&gasPrice)
	api.e.lock.Unlock()

	api.e.txPool.SetGasTip((*big.Int)(&gasPrice))
	api.e.Miner().SetGasTip((*big.Int)(&gasPrice))
	return true
}

// SetGasLimit sets the gaslimit to target towards during mining.
func (api *MinerAPI) SetGasLimit(gasLimit hexutil.Uint64) bool {
	api.e.Miner().SetGasCeil(uint64(gasLimit))
	return true
}
