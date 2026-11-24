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

	"github.com/ethereum/go-ethereum/qkc/wire"
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

// NewMasterSlaveTransport creates the peer-ID frame transport. A zero payload
// limit uses the protocol maximum. On error the caller retains ownership of nc.
func NewMasterSlaveTransport(nc net.Conn, maxPayloadSize uint32) (FrameTransport, error) {
	if nc == nil {
		return nil, fmt.Errorf("master-slave connection must not be nil")
	}
	maxPayloadSize, err := ClusterPayloadSize(maxPayloadSize)
	if err != nil {
		return nil, err
	}
	return NewTCPTransport(nc, func(r io.Reader) (*wire.ClusterFrame, error) {
		return wire.ReadClusterFrameWithPeerID(r, maxPayloadSize)
	}, wire.WriteClusterFrameWithPeerID), nil
}

// NewXshardTransport creates a slave-to-slave frame transport without peer IDs.
// On error the caller retains ownership of nc.
func NewXshardTransport(nc net.Conn, maxPayloadSize uint32) (FrameTransport, error) {
	if nc == nil {
		return nil, fmt.Errorf("xshard connection must not be nil")
	}
	maxPayloadSize, err := ClusterPayloadSize(maxPayloadSize)
	if err != nil {
		return nil, err
	}
	return NewTCPTransport(nc, func(r io.Reader) (*wire.ClusterFrame, error) {
		return wire.ReadClusterFrame(r, maxPayloadSize)
	}, wire.WriteClusterFrame), nil
}

// ValidateSlaveChainID checks that an identity is a positive uint256.
func ValidateSlaveChainID(id *big.Int) error {
	if id == nil || id.Sign() <= 0 || id.BitLen() > 256 {
		return fmt.Errorf("invalid slave chain ID: %v", id)
	}
	return nil
}

// ClusterPayloadSize resolves the default payload size and enforces the
// cluster protocol limit.
func ClusterPayloadSize(size uint32) (uint32, error) {
	if size == 0 {
		return wire.MaxFramePayloadLen, nil
	}
	if size > wire.MaxFramePayloadLen {
		return 0, fmt.Errorf("max payload size %d exceeds protocol limit %d", size, wire.MaxFramePayloadLen)
	}
	return size, nil
}
