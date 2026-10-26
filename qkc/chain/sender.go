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

// Package chain defines typed outbound boundaries used by chain business logic.
package chain

import (
	"context"

	"github.com/ethereum/go-ethereum/qkc/wire"
)

// Sender is the chain-side application port for outbound communication. The
// chain process composition owns the connections and implements these methods;
// chain services receive only this interface.
//
// Every method names a protocol operation and uses its concrete message types.
// The implementation handles opcode selection, serialization, request
// correlation, and route selection.
type Sender interface {
	// ── Chain → Master reports ──

	// SendMinorBlockHeaderToMaster and SendMinorBlockHeaderListToMaster report
	// new minor block headers to the master.
	SendMinorBlockHeaderToMaster(ctx context.Context, req *wire.AddMinorBlockHeaderRequest) (*wire.AddMinorBlockHeaderResponse, error)
	SendMinorBlockHeaderListToMaster(ctx context.Context, req *wire.AddMinorBlockHeaderListRequest) (*wire.AddMinorBlockHeaderListResponse, error)

	// ── Chain → chain mesh (xshard deposits) ──

	// SendXshardTxList broadcasts req to the connections serving branch;
	// chainID is the routing key.
	SendXshardTxListToChain(ctx context.Context, chainID uint32, req *wire.AddXshardTxListRequest) error
	SendBatchXshardTxListToChain(ctx context.Context, chainID uint32, req *wire.BatchAddXshardTxListRequest) error

	// Deferred: virtual-peer RPCs and commands that use PeerConn. Enable after
	// the peer message types and virtual-peer routing are implemented.
	/*

		GetMinorBlockHeaderListFromPeer(ctx context.Context, peerID uint64, req *wire.GetMinorBlockHeaderListRequest) (*wire.GetMinorBlockHeaderListResponse, error)
		GetMinorBlockHeaderListWithSkipFromPeer(ctx context.Context, peerID uint64, req *wire.GetMinorBlockHeaderListWithSkipRequest) (*wire.GetMinorBlockHeaderListResponse, error)
		GetMinorBlockListFromPeer(ctx context.Context, peerID uint64, req *wire.GetMinorBlockListRequest) (*wire.GetMinorBlockListResponse, error)

		// ── Chain → virtual peer commands ──

		// SendNewBlockToPeer sends a block to one peer. BroadcastNewBlockToPeers
		// sends it to all virtual peers attached to this chain.
		SendNewBlockToPeer(ctx context.Context, peerID uint64, command *wire.NewBlockMinorCommand) error
		BroadcastNewBlockToPeers(ctx context.Context, command *wire.NewBlockMinorCommand) error

		// SendNewMinorBlockHeaderListToPeer sends one peer a new-tip/header
		// announcement. BroadcastNewMinorBlockHeaderListToPeers announces it to all
		// virtual peers attached to this chain.
		SendNewMinorBlockHeaderListToPeer(ctx context.Context, peerID uint64, command *wire.NewMinorBlockHeaderListCommand) error
		BroadcastNewMinorBlockHeaderListToPeers(ctx context.Context, command *wire.NewMinorBlockHeaderListCommand) error

		// SendNewTransactionListToPeer sends one peer a transaction announcement.
		// BroadcastNewTransactionListToPeers excludes exceptPeerID when it is nonzero,
		// so a transaction is not echoed to its source peer.
		SendNewTransactionListToPeer(ctx context.Context, peerID uint64, command *wire.NewTransactionListCommand) error
		BroadcastNewTransactionListToPeers(ctx context.Context, exceptPeerID uint64, command *wire.NewTransactionListCommand) error
	*/
}
