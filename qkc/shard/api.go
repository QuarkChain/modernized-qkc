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

package shard

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// MiningBackend provides remote mining for a single shard. Implementations must
// support concurrent RPC calls and reject invalid, stale or unknown submissions.
// It is an optional engine capability, separate from consensus.Engine.
type MiningBackend interface {
	GetWork() ([4]string, error)
	SubmitWork(nonce types.BlockNonce, hash, digest common.Hash) bool
}

// MiningAPI exposes eth_getWork and eth_submitWork for one shard backend.
type MiningAPI struct {
	backend MiningBackend
}

// NewMiningAPI binds mining requests to backend. Register it in the eth namespace
// on that shard's RPC endpoint. A nil backend reports mining as unsupported.
func NewMiningAPI(backend MiningBackend) *MiningAPI {
	return &MiningAPI{backend: backend}
}

// APIs returns the mining RPC service bound to this shard's engine. The node
// registers these APIs on the shard's endpoint, alongside its other eth APIs.
func (c *ShardChain) APIs() []rpc.API {
	backend, _ := c.chain.Engine().(MiningBackend)
	return []rpc.API{{Namespace: "eth", Service: NewMiningAPI(backend)}}
}

// GetWork returns the seal hash, DAG seed hash, target and block number as hex
// strings, preserving the Geth work format. Unavailable work is an RPC error.
func (api *MiningAPI) GetWork() ([4]string, error) {
	if api.backend == nil {
		return [4]string{}, errors.New("mining not supported")
	}
	return api.backend.GetWork()
}

// SubmitWork forwards the fixed-width nonce, seal hash and mix digest. Rejected
// submissions, including those on an unsupported backend, return false.
func (api *MiningAPI) SubmitWork(nonce types.BlockNonce, hash, digest common.Hash) bool {
	if api.backend == nil {
		return false
	}
	return api.backend.SubmitWork(nonce, hash, digest)
}
