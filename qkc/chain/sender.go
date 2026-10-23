// Copyright 2026-2027, QuarkChain.

// Package chain defines typed outbound boundaries used by chain business logic.
// PeerConn traffic is deferred in Sender until its wire messages are added.
package chain

import (
	"context"

	"github.com/ethereum/go-ethereum/qkc/wire"
)

// Sender is the chain-side application port for Master communication. The
// chain process composition owns the connection and implements these methods;
// chain services receive only this interface.
//
// Every method names a protocol operation and uses its concrete message types.
// The implementation handles opcode selection, serialization, request
// correlation, and route selection.
type Sender interface {
	// ── Chain → Master reports ──

	// SendMinorBlockHeaderToMaster reports one accepted minor block header and
	// returns the master's response (which may carry updated chain settings).
	SendMinorBlockHeaderToMaster(ctx context.Context, req *wire.AddMinorBlockHeaderRequest) (*wire.AddMinorBlockHeaderResponse, error)
	// SendMinorBlockHeaderListToMaster reports a batch of accepted headers.
	SendMinorBlockHeaderListToMaster(ctx context.Context, req *wire.AddMinorBlockHeaderListRequest) (*wire.AddMinorBlockHeaderListResponse, error)

	// ── Chain → chain mesh (xshard deposits) ──

	// SendXshardTxListToChain sends cross-chain deposits to the destination
	// chain. chainID is the destination route key. A nonzero remote error_code
	// is returned as an error; callers do not consume the response payload.
	SendXshardTxListToChain(ctx context.Context, chainID uint32, req *wire.AddXshardTxListRequest) error
	// SendBatchXshardTxListToChain sends a batch of cross-chain deposits to the
	// destination chain. A nonzero remote error_code is returned as an error.
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
