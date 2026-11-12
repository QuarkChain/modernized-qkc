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

package wire

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

//go:generate sh -c "go tool ssz gen --package github.com/ethereum/go-ethereum/qkc/wire --disable-delegation --type-names `sed -n 's/^type \\([^ ]*\\) struct.*/\\1/p' messages.go | paste -sd, -` --output messages_ssz.go"
//go:generate go tool goimports -w messages_ssz.go

// =============================================================================
// Wire-level constants
// =============================================================================
//
// These match the fixed-width fields in the message schemas.

// UInt128Length is the byte length of a uint128.
const UInt128Length = 16

// Data carries an already encoded object unchanged through SSZ: RLP for geth
// objects, SSZ for QKC root objects.
type Data []byte

// =============================================================================
// §1  Cluster initialisation
// =============================================================================

// The large ssz-max tags below are schema bounds, not permitted frame sizes.
// MaxFramePayloadLen caps network payloads, including single Data fields such
// as RootTip. Generated dynamic-list decoders validate the offset table
// against the actual input before allocating elements.

// PingRequest (ClusterOp.PING, 0x81) initializes a slave connection.
// Slave↔slave PINGs use HasRootTip=false. pyquarkchain sent a dummy root block
// on these links, but the receiver ignored it.
type PingRequest struct {
	// ChainID identifies the chain hosted by the dialing slave on
	// slave↔slave links, whose frames have no metadata. It encodes
	// ChainConfig.ChainID as a little-endian uint256. Master→slave
	// handlers ignore it.
	ChainID    uint256.Int
	HasRootTip bool
	RootTip    Data `ssz-max:"4294967295"` // SSZ-encoded RootBlock when HasRootTip is true.
}

// PongResponse (ClusterOp.PONG, 0x82) is the slave's response to PING and
// echoes the request RPCID in the frame header.
type PongResponse struct {
	ChainID uint256.Int
}

// SlaveInfo describes a slave endpoint and the chain it hosts.
type SlaveInfo struct {
	Host    []byte `ssz-max:"4294967295"`
	Port    uint16
	ChainID uint256.Int
}

// ConnectToSlavesRequest (opcode 0x83) asks a slave to connect to the listed
// slaves and report each result.
// Master must send this request to one slave at a time and wait for its response
// before sending to the next. Together with inbound registration before PONG,
// this ensures each slave pair establishes a single connection.
type ConnectToSlavesRequest struct {
	SlaveInfoList []*SlaveInfo `ssz-max:"4294967295"`
}

// ConnectToSlavesResponse (opcode 0x84) reports one result per requested
// slave, in request order. Empty means success; non-empty contains the error
// payload.
type ConnectToSlavesResponse struct {
	ResultList [][]byte `ssz-max:"4294967295,4294967295" ssz-size:"?,?"`
}

// HelloCommand (CommandOp.HELLO, 0x00) is the non-RPC Master↔Master handshake.
type HelloCommand struct {
	Version              uint32
	NetworkID            uint64
	PeerID               common.Hash
	PeerIP               [UInt128Length]byte
	PeerPort             uint16
	RootBlockHeader      Data `ssz-max:"4294967295"` // SSZ-encoded RootBlockHeader.
	GenesisRootBlockHash common.Hash
}

// AddShardBlockHeaderRequest reports an accepted shard block to Master.
// ShardBlockHeader carries an RLP-encoded geth header.
type AddShardBlockHeaderRequest struct {
	ShardBlockHeader Data        `ssz-max:"4294967295"` // minor_block_header in pyquarkchain
	CoinbaseAmount   uint256.Int // Shard miner's total reward in QKC base units, including fees.
}

type AddShardBlockHeaderResponse struct {
	ErrorCode uint32
}

// AddXshardTxListRequest delivers one source block's deposits to a destination
// shard, including empty lists for the inbox cursor. Each item is RLP(DepositTx),
// without the 0x7e transaction prefix, in source log order.
type AddXshardTxListRequest struct {
	ChainID        uint256.Int // Destination ChainConfig.ChainID, little-endian.
	ShardBlockHash common.Hash // Source block hash (minor_block_hash in pyquarkchain).
	DepositTxList  []Data      `ssz-max:"4294967295"`
}

type AddXshardTxListResponse struct {
	ErrorCode uint32
}

// AddRootBlockRequest sends a root block from Master to every slave. RootBlock
// carries an SSZ-encoded QKC RootBlock.
type AddRootBlockRequest struct {
	RootBlock Data `ssz-max:"4294967295"`
}

type AddRootBlockResponse struct {
	ErrorCode uint32
}

// GetUnconfirmedHeadersRequest asks a slave for its unconfirmed shard headers.
type GetUnconfirmedHeadersRequest struct{}

// GetUnconfirmedHeadersResponse returns unconfirmed headers for the chain
// served by this slave connection. Headers are encoded as RLP.
type GetUnconfirmedHeadersResponse struct {
	ErrorCode  uint32
	HeaderList []Data `ssz-max:"4294967295"`
}

// GetRootChainStakesRequest asks a chain for the stakes recorded for Address
// at ShardBlockHash.
type GetRootChainStakesRequest struct {
	Address        common.Address
	ShardBlockHash common.Hash // minor_block_hash in pyquarkchain
}

// GetRootChainStakesResponse reports staking information. Stakes is a
// little-endian uint256; Signer is a 20-byte address.
type GetRootChainStakesResponse struct {
	ErrorCode uint32
	Stakes    uint256.Int
	Signer    common.Address
}
