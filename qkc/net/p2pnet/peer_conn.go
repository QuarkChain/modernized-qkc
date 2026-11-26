package p2pnet

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/qkc/net/conn"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// PeerRequestHandler serves root-chain traffic received from peers. Calls may
// run concurrently. Errors close the peer connection. peerID is the locally
// assigned cluster peer ID; all payloads are decoded before calling the handler.
type PeerRequestHandler interface {
	NewTip(peerID uint64, rootTip *qkctypes.RootBlockHeader) error
	NewTransactions(peerID uint64, transactions types.Transactions) error
	GetRootHeaders(peerID uint64, hash common.Hash, height *big.Int, limit, skip uint32, reverse bool) (*RootHeadersResult, error)
	GetRootBlocks(peerID uint64, hashes []common.Hash) ([]*qkctypes.RootBlock, error)
}

type PeerConnConfig struct {
	Remote        *p2p.Peer
	ReadWriter    p2p.MsgReadWriter
	ClusterPeerID uint64
	Handler       PeerRequestHandler
	Logger        log.Logger

	// Forwarder receives shard traffic as internal frames. chainID selects the
	// destination slave; the frame carries this connection's cluster peer ID and
	// the original opcode, RPC ID, and payload without decoding it.
	// It must hand off frames without blocking the reader goroutine. An error
	// closes the peer connection.
	Forwarder func(chainID *big.Int, frame *wire.ClusterFrame) error
}

// PeerConn sends and receives frames over one established geth peer.
type PeerConn struct {
	*conn.BaseConn

	remoteID      enode.ID
	clusterPeerID uint64
	handler       PeerRequestHandler
}

// NewPeerConn wraps a geth peer without starting it. On error the caller retains
// ownership of the transport. The caller supplies a nonzero cluster peer ID.
// The caller must complete the HELLO exchange and service-level admission checks
// before allocating the ID, constructing this connection, and calling Start.
// The service owns the handshake: both sides send HELLO and concurrently read
// the first incoming frame, which must be a root-chain HELLO with RPC ID zero.
// An overall timeout covers sending and receiving; failure or invalid HELLO
// disconnects the geth peer. This uses the protocol MsgReadWriter directly,
// before PeerConn starts reading. The service must also create internal virtual
// shard connections before starting and publishing PeerConn. Keeping this setup
// in the service avoids handshake-state checks in the business handlers.
func NewPeerConn(cfg PeerConnConfig) (*PeerConn, error) {
	if cfg.ClusterPeerID == 0 {
		return nil, errors.New("cluster peer ID must not be zero")
	}
	if cfg.Forwarder == nil {
		return nil, errors.New("peer forwarder must not be nil")
	}
	if cfg.Handler == nil {
		return nil, errors.New("peer handler must not be nil")
	}

	transport, err := NewTransport(cfg.Remote, cfg.ReadWriter)
	if err != nil {
		return nil, err
	}
	peer := &PeerConn{
		remoteID:      cfg.Remote.ID(),
		clusterPeerID: cfg.ClusterPeerID,
		handler:       cfg.Handler,
	}
	peer.BaseConn = conn.NewBaseConn(conn.Config{
		Transport:   transport,
		Serializers: conn.P2PSerializers(),
		NonRPCOps:   conn.P2PNonRPCOps(),
		Handlers: map[byte]conn.TypedHandler{
			byte(wire.CommandOpNewTip):                peer.handleNewTip,
			byte(wire.CommandOpNewTransactions):       peer.handleNewTransactions,
			byte(wire.CommandOpGetRootHeadersRequest): peer.handleGetRootHeaders,
			byte(wire.CommandOpGetRootBlocksRequest):  peer.handleGetRootBlocks,
		},
		Forwarder: func(value wire.Frame) bool {
			frame := value.(*wire.P2PFrame)
			if frame.ChainID.Sign() == 0 {
				return false
			}
			forwarded := &wire.ClusterFrame{
				ClusterPeerID: peer.clusterPeerID,
				Opcode:        frame.Opcode,
				RPCID:         frame.RPCID,
				Payload:       frame.Payload,
			}
			if err := cfg.Forwarder(frame.ChainID, forwarded); err != nil {
				peer.Logger().Warn("peer frame forwarding failed", "peer", peer.remoteID, "err", err)
				peer.Close()
			}
			return true
		},
		Logger: cfg.Logger,
	})
	return peer, nil
}

func (peer *PeerConn) ID() enode.ID { return peer.remoteID }

// ClusterPeerID returns the caller-assigned ID used for routing within the cluster.
func (peer *PeerConn) ClusterPeerID() uint64 { return peer.clusterPeerID }

// SendNewTip announces the master's root tip with ChainID zero and no shard tip.
// Slaves announce both their root and shard tips through internal communication;
// the master relays those announcements with the slave's ChainID via WriteFrame.
func (peer *PeerConn) SendNewTip(rootTip *qkctypes.RootBlockHeader) error {
	if rootTip == nil {
		return errors.New("root tip must not be nil")
	}
	encoded, err := rootTip.MarshalSSZ()
	if err != nil {
		return fmt.Errorf("encode root tip: %w", err)
	}
	return peer.SendP2PCommand(wire.RootChainID(), byte(wire.CommandOpNewTip), &wire.NewTipCommand{RootTip: encoded})
}

// SendNewTransactions broadcasts transactions accepted by the master.
// This supports pyquarkchain's centralized transaction ingress, where the master
// submits transactions to local slaves before broadcasting them to remote masters.
// Whether this path is needed remains undecided: shards have their own RPC
// endpoints, and the master's RPC may serve only root-chain operations. If all
// transaction ingress and broadcasting stay on slaves, this method is unnecessary.
func (peer *PeerConn) SendNewTransactions(transactions types.Transactions) error {
	for index, transaction := range transactions {
		if transaction == nil {
			return fmt.Errorf("nil transaction at index %d", index)
		}
	}
	encoded, err := rlp.EncodeToBytes(transactions)
	if err != nil {
		return fmt.Errorf("encode transactions: %w", err)
	}
	return peer.SendP2PCommand(wire.RootChainID(), byte(wire.CommandOpNewTransactions), &wire.NewTransactionsCommand{Transactions: encoded})
}

// SendGetRootHeaders queries by hash when height is nil. Otherwise hash must be
// zero and height must be a nonnegative uint256. Reverse selects genesis direction.
func (peer *PeerConn) SendGetRootHeaders(ctx context.Context, hash common.Hash, height *big.Int, limit, skip uint32, reverse bool) (*RootHeadersResult, error) {
	request := &wire.GetRootHeadersRequest{
		Type: wire.HeaderOriginHash, Data: hash, Limit: limit, Skip: skip, Direction: uint8(wire.DirectionTip),
	}
	if reverse {
		request.Direction = uint8(wire.DirectionGenesis)
	}
	if height != nil {
		if hash != (common.Hash{}) || height.Sign() < 0 || height.BitLen() > 256 {
			return nil, errors.New("root header origin must be a hash or a nonnegative uint256 height")
		}
		request.Type = wire.HeaderOriginHeight
		height.FillBytes(request.Data[:])
	}
	response, err := peer.SendP2PRPC(ctx, wire.RootChainID(), byte(wire.CommandOpGetRootHeadersRequest), request)
	if err != nil {
		return nil, err
	}
	result, ok := response.(*wire.GetRootHeadersResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected root headers response %T", response)
	}
	decoded := &RootHeadersResult{RootTip: new(qkctypes.RootBlockHeader), Headers: make([]*qkctypes.RootBlockHeader, len(result.Headers))}
	if err := decoded.RootTip.UnmarshalSSZ(result.RootTip); err != nil {
		return nil, fmt.Errorf("decode root tip: %w", err)
	}
	for index, encoded := range result.Headers {
		decoded.Headers[index] = new(qkctypes.RootBlockHeader)
		if err := decoded.Headers[index].UnmarshalSSZ(encoded); err != nil {
			return nil, fmt.Errorf("decode root header %d: %w", index, err)
		}
	}
	return decoded, nil
}

func (peer *PeerConn) SendGetRootBlocks(ctx context.Context, hashes []common.Hash) ([]*qkctypes.RootBlock, error) {
	response, err := peer.SendP2PRPC(ctx, wire.RootChainID(), byte(wire.CommandOpGetRootBlocksRequest), &wire.GetRootBlocksRequest{Hashes: hashes})
	if err != nil {
		return nil, err
	}
	result, ok := response.(*wire.GetRootBlocksResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected root blocks response %T", response)
	}
	blocks := make([]*qkctypes.RootBlock, len(result.Blocks))
	for index, encoded := range result.Blocks {
		blocks[index] = new(qkctypes.RootBlock)
		if err := blocks[index].UnmarshalSSZ(encoded); err != nil {
			return nil, fmt.Errorf("decode root block %d: %w", index, err)
		}
	}
	return blocks, nil
}

func (peer *PeerConn) handleNewTip(req wire.SSZMessage) (wire.SSZMessage, error) {
	command := req.(*wire.NewTipCommand)
	if len(command.ShardTip) != 0 {
		return nil, errors.New("root-chain tip must omit shard tip")
	}
	rootTip := new(qkctypes.RootBlockHeader)
	if err := rootTip.UnmarshalSSZ(command.RootTip); err != nil {
		return nil, fmt.Errorf("decode root tip: %w", err)
	}
	return nil, peer.handler.NewTip(peer.clusterPeerID, rootTip)
}

func (peer *PeerConn) handleNewTransactions(req wire.SSZMessage) (wire.SSZMessage, error) {
	command := req.(*wire.NewTransactionsCommand)
	var transactions types.Transactions
	if err := rlp.DecodeBytes(command.Transactions, &transactions); err != nil {
		return nil, fmt.Errorf("decode transactions: %w", err)
	}
	return nil, peer.handler.NewTransactions(peer.clusterPeerID, transactions)
}

func (peer *PeerConn) handleGetRootHeaders(req wire.SSZMessage) (wire.SSZMessage, error) {
	request := req.(*wire.GetRootHeadersRequest)
	if request.Type != wire.HeaderOriginHash && request.Type != wire.HeaderOriginHeight {
		return nil, errors.New("invalid root header origin type")
	}
	direction := wire.Direction(request.Direction)
	if direction != wire.DirectionGenesis && direction != wire.DirectionTip {
		return nil, errors.New("invalid root header direction")
	}
	hash := request.Data
	var height *big.Int
	if request.Type == wire.HeaderOriginHeight {
		height = new(big.Int).SetBytes(request.Data[:])
		hash = common.Hash{}
	}
	result, err := peer.handler.GetRootHeaders(peer.clusterPeerID, hash, height, request.Limit, request.Skip, direction == wire.DirectionGenesis)
	if err != nil {
		return nil, err
	}
	if result == nil || result.RootTip == nil {
		return nil, errors.New("root headers handler returned no root tip")
	}
	encoded, err := result.RootTip.MarshalSSZ()
	if err != nil {
		return nil, fmt.Errorf("encode root tip: %w", err)
	}
	response := &wire.GetRootHeadersResponse{RootTip: encoded, Headers: make([]wire.Data, len(result.Headers))}
	for index, header := range result.Headers {
		if header == nil {
			return nil, fmt.Errorf("nil root header at index %d", index)
		}
		response.Headers[index], err = header.MarshalSSZ()
		if err != nil {
			return nil, fmt.Errorf("encode root header %d: %w", index, err)
		}
	}
	return response, nil
}

func (peer *PeerConn) handleGetRootBlocks(req wire.SSZMessage) (wire.SSZMessage, error) {
	request := req.(*wire.GetRootBlocksRequest)
	blocks, err := peer.handler.GetRootBlocks(peer.clusterPeerID, request.Hashes)
	if err != nil {
		return nil, err
	}
	response := &wire.GetRootBlocksResponse{Blocks: make([]wire.Data, len(blocks))}
	for index, block := range blocks {
		if block == nil {
			return nil, fmt.Errorf("nil root block at index %d", index)
		}
		response.Blocks[index], err = block.MarshalSSZ()
		if err != nil {
			return nil, fmt.Errorf("encode root block %d: %w", index, err)
		}
	}
	return response, nil
}
