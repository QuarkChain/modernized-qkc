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
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

// The go:generate directive below runs from this package directory; it cds to
// the repository root so the generator paths are stable and can be copied
// into documentation.
//go:generate sh -c "cd ../.. && go run github.com/ferranbt/fastssz/sszgen --path qkc/wire/messages.go --output qkc/wire/messages_ssz.go"

// =============================================================================
// Wire-level constants
// =============================================================================
//
// These match the fixed-width fields in the message schemas.

// HashLength is the byte length of a hash256.
const HashLength = 32

// SignatureLength is the byte length of a signature65.
const SignatureLength = 65

// UInt128Length is the byte length of a uint128.
const UInt128Length = 16

// =============================================================================
// §1  Cluster initialisation
// =============================================================================

// TODO(ssz-limits): The 4294967295 values below are broad numeric bounds
// supplied to fastssz for variable-length fields, not intended 4 GB message
// sizes. Actual message size is primarily constrained by the per-frame size
// limit. Revisit these bounds if QKC defines protocol-specific field limits.

// PingRequest (ClusterOp.PING, 0x81) initializes a chain connection.
// Chain↔chain PINGs use HasRootTip=false. pyquarkchain sent a dummy root block
// on these links, but the receiver ignored it.
type PingRequest struct {
	// ChainID identifies the dialing chain on chain↔chain links, whose frames
	// have no metadata. Master→chain handlers ignore it; zero is a valid chain
	// ID and does not mean root.
	ChainID    uint32
	HasRootTip bool
	RootTip    []byte `ssz-max:"4294967295"` // SSZ encoding of RootBlock when HasRootTip is true.
}

// PongResponse (ClusterOp.PONG, 0x82) is the chain's response to PING and
// echoes the request RPCID in the frame header.
type PongResponse struct {
	ChainID uint32
}

// ChainInfo describes a remote chain for ConnectToChainsRequest.
type ChainInfo struct {
	Host    []byte `ssz-max:"4294967295"`
	Port    uint16
	ChainID uint32
}

// ConnectToChainsRequest (opcode 0x83) asks a chain to connect to the listed
// chains and report each result.
type ConnectToChainsRequest struct {
	ChainInfoList []ChainInfo `ssz-max:"4294967295"`
}

// ConnectToChainsResponse (opcode 0x84) reports one result per requested
// chain, in request order. Empty means success; non-empty contains the error
// payload.
type ConnectToChainsResponse struct {
	ResultList [][]byte `ssz-max:"4294967295,4294967295" ssz-size:"?,?"`
}

// HelloCommand (CommandOp.HELLO, 0x00) is the non-RPC Master↔Master handshake.
type HelloCommand struct {
	Version              uint32
	NetworkID            uint32
	PeerID               [HashLength]byte
	PeerIP               [UInt128Length]byte
	PeerPort             uint16
	RootBlockHeader      []byte `ssz-max:"4294967295"` // SSZ encoding of RootBlockHeader.
	GenesisRootBlockHash [HashLength]byte
}

// ArtificialTxConfig carries the current mining target intervals from Master.
type ArtificialTxConfig struct {
	TargetRootBlockTime  uint32
	TargetMinorBlockTime uint32
}

// ShardStats accompanies each accepted chain block so Master can update its
// per-chain status. The legacy branch is represented by the chain ID.
type ShardStats struct {
	ChainID            uint32
	Height             uint64
	Difficulty         uint256.Int
	CoinbaseAddress    common.Address
	Timestamp          uint64
	TxCount60s         uint32
	PendingTxCount     uint32
	TotalTxCount       uint32
	BlockCount60s      uint32
	StaleBlockCount60s uint32
	LastBlockTime      uint32
}

// AddMinorBlockHeaderRequest reports an accepted chain block to Master. The
// Ethereum header carries the QuarkChain fields in its Extra data.
type AddMinorBlockHeaderRequest struct {
	BlockHeader    *types.Header
	TxCount        uint32
	XShardTxCount  uint32
	CoinbaseAmount uint256.Int
	ShardStats     ShardStats
}

// AddMinorBlockHeaderResponse returns the current mining target intervals.
type AddMinorBlockHeaderResponse struct {
	ErrorCode          uint32
	ArtificialTxConfig ArtificialTxConfig
}

type AddMinorBlockHeaderListRequest struct {
	BlockHeaders    []types.Header `ssz-max:"4294967295"`
	CoinbaseAmounts []uint256.Int  `ssz-max:"4294967295"`
}

type AddMinorBlockHeaderListResponse struct {
	ErrorCode uint32
}

// AddXshardTxListRequest delivers the deposits emitted by one source block to
// one destination chain. An empty, present list means that block sent this
// chain no deposits; the receiver must still record it for the inbox cursor.
// Each deposit has its own format version; the legacy list version is not part
// of this new protocol.
type AddXshardTxListRequest struct {
	ChainID        uint32
	MinorBlockHash [HashLength]byte
	Deposits       []types.DepositTx `ssz-max:"4294967295"`
}

type AddXshardTxListResponse struct {
	ErrorCode uint32
}

type BatchAddXshardTxListRequest struct {
	AddXshardTxListRequestList []AddXshardTxListRequest `ssz-max:"4294967295"`
}

type BatchAddXshardTxListResponse struct {
	ErrorCode uint32
}
