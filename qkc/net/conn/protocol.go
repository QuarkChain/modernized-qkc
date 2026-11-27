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

package conn

import (
	"fmt"
	"io"
	"math/big"
	"net"

	"github.com/ethereum/go-ethereum/qkc/net/wire"
)

// ClusterSerializers returns an independent request/response registry shared by
// all cluster connection types. Handlers define which requests each side accepts.
func ClusterSerializers() map[byte]*OpSerializer {
	return map[byte]*OpSerializer{
		byte(wire.ClusterOpPing):                   OpSerializerFor[wire.PingRequest, wire.PongResponse](byte(wire.ClusterOpPong)),
		byte(wire.ClusterOpConnectToSlavesRequest): OpSerializerFor[wire.ConnectToSlavesRequest, wire.ConnectToSlavesResponse](byte(wire.ClusterOpConnectToSlavesResponse)),

		byte(wire.ClusterOpAddRootBlockRequest):          OpSerializerFor[wire.AddRootBlockRequest, wire.AddRootBlockResponse](byte(wire.ClusterOpAddRootBlockResponse)),
		byte(wire.ClusterOpGetUnconfirmedHeadersRequest): OpSerializerFor[wire.GetUnconfirmedHeadersRequest, wire.GetUnconfirmedHeadersResponse](byte(wire.ClusterOpGetUnconfirmedHeadersResponse)),
		byte(wire.ClusterOpGetRootChainStakesRequest):    OpSerializerFor[wire.GetRootChainStakesRequest, wire.GetRootChainStakesResponse](byte(wire.ClusterOpGetRootChainStakesResponse)),
		byte(wire.ClusterOpAddShardBlockHeaderRequest):   OpSerializerFor[wire.AddShardBlockHeaderRequest, wire.AddShardBlockHeaderResponse](byte(wire.ClusterOpAddShardBlockHeaderResponse)),
		byte(wire.ClusterOpAddXshardTxListRequest):       OpSerializerFor[wire.AddXshardTxListRequest, wire.AddXshardTxListResponse](byte(wire.ClusterOpAddXshardTxListResponse)),
	}
}

// P2PSerializers returns an independent registry for peer commands and RPCs
// after handshake. HELLO is excluded because the service exchanges and validates
// it directly before constructing PeerConn.
func P2PSerializers() map[byte]*OpSerializer {
	return map[byte]*OpSerializer{
		byte(wire.CommandOpNewTip):                 OpSerializerFor[wire.NewTipCommand, wire.NewTipCommand](byte(wire.CommandOpNewTip)),
		byte(wire.CommandOpNewTransactions):        OpSerializerFor[wire.NewTransactionsCommand, wire.NewTransactionsCommand](byte(wire.CommandOpNewTransactions)),
		byte(wire.CommandOpGetRootHeadersRequest):  OpSerializerFor[wire.GetRootHeadersRequest, wire.GetRootHeadersResponse](byte(wire.CommandOpGetRootHeadersResponse)),
		byte(wire.CommandOpGetRootBlocksRequest):   OpSerializerFor[wire.GetRootBlocksRequest, wire.GetRootBlocksResponse](byte(wire.CommandOpGetRootBlocksResponse)),
		byte(wire.CommandOpGetShardHeadersRequest): OpSerializerFor[wire.GetShardHeadersRequest, wire.GetShardHeadersResponse](byte(wire.CommandOpGetShardHeadersResponse)),
		byte(wire.CommandOpGetShardBlocksRequest):  OpSerializerFor[wire.GetShardBlocksRequest, wire.GetShardBlocksResponse](byte(wire.CommandOpGetShardBlocksResponse)),
	}
}

// P2PNonRPCOps returns the peer opcodes that do not expect a response after
// handshake. HELLO is excluded for the same reason as in P2PSerializers.
func P2PNonRPCOps() map[byte]struct{} {
	return map[byte]struct{}{
		byte(wire.CommandOpNewTip):          {},
		byte(wire.CommandOpNewTransactions): {},
	}
}

// NewMasterSlaveTransport creates the peer-ID frame transport for ordinary
// cluster traffic without a configurable payload limit.
// On error the caller retains nc.
func NewMasterSlaveTransport(nc net.Conn) (FrameTransport, error) {
	if nc == nil {
		return nil, fmt.Errorf("master-slave connection must not be nil")
	}
	return NewTCPTransport(nc, func(r io.Reader) (wire.Frame, error) {
		return wire.ReadClusterFrameWithPeerID(r, 0)
	}, func(w io.Writer, frame wire.Frame) error {
		return wire.WriteClusterFrameWithPeerID(w, frame, 0)
	}), nil
}

// NewXshardTransport creates an internal slave-to-slave frame transport without
// peer IDs or a payload limit. On error the caller retains ownership of nc.
func NewXshardTransport(nc net.Conn) (FrameTransport, error) {
	if nc == nil {
		return nil, fmt.Errorf("xshard connection must not be nil")
	}
	return NewTCPTransport(nc, func(r io.Reader) (wire.Frame, error) {
		return wire.ReadClusterFrame(r)
	}, wire.WriteClusterFrame), nil
}

// ValidateSlaveChainID checks that an identity is a positive uint256.
func ValidateSlaveChainID(id *big.Int) error {
	if id == nil || id.Sign() <= 0 || id.BitLen() > 256 {
		return fmt.Errorf("invalid slave chain ID: %v", id)
	}
	return nil
}
