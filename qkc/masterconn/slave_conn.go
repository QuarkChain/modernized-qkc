// Package masterconn implements master-side cluster connections.
package masterconn

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/qkc/conn"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/qkc/wire"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// SlaveRequestHandler serves requests received from slaves. Calls may run
// concurrently. Return *comm.RemoteError for business rejections; other errors
// close the connection. sourceChainID is an independent copy of the configured
// remote chain ID. The owner completes PING identity verification, slave
// interconnection, and cluster initialization before starting business activity;
// this connection does not maintain a separate handshake gate for handlers.
type SlaveRequestHandler interface {
	AddShardBlockHeader(sourceChainID *big.Int, header *types.Header, coinbaseAmount *uint256.Int) error
}

// SlaveConnConfig configures the master's connection to a slave.
type SlaveConnConfig struct {
	Conn           net.Conn
	RemoteChainID  *big.Int
	MaxPayloadSize uint32
	Handler        SlaveRequestHandler
	Logger         log.Logger
}

// SlaveConn wraps one master-to-slave TCP connection. The owner establishes the
// TCP connection, starts it, and verifies the slave with SendPing. The
// owner watches WaitUntilClosed and decides whether to stop the master.
type SlaveConn struct {
	*conn.BaseConn

	remoteChainID *big.Int
	handler       SlaveRequestHandler
}

// NewSlaveConn wraps a TCP connection without starting it. On error the caller
// retains ownership of cfg.Conn. RemoteChainID must be positive and fit in
// uint256. A zero MaxPayloadSize uses the protocol maximum.
func NewSlaveConn(cfg SlaveConnConfig) (*SlaveConn, error) {
	if cfg.Conn == nil {
		return nil, errors.New("slave connection must not be nil")
	}
	if cfg.Handler == nil {
		return nil, errors.New("slave handler must not be nil")
	}
	if cfg.RemoteChainID == nil || cfg.RemoteChainID.Sign() <= 0 || cfg.RemoteChainID.BitLen() > 256 {
		return nil, fmt.Errorf("invalid slave chain ID: %v", cfg.RemoteChainID)
	if err := conn.ValidateSlaveChainID(cfg.RemoteChainID); err != nil {
		return nil, err
	}
	transport, err := conn.NewMasterSlaveTransport(cfg.Conn, cfg.MaxPayloadSize)
	if err != nil {
		return nil, err
	}

	sc := &SlaveConn{
		remoteChainID: new(big.Int).Set(cfg.RemoteChainID),
		handler:       cfg.Handler,
	}
	sc.BaseConn = conn.NewBaseConn(conn.Config{
		Transport:   transport,
		Serializers: conn.ClusterSerializers(),
		Handlers: map[byte]conn.TypedHandler{
			byte(wire.ClusterOpAddShardBlockHeaderRequest): sc.handleAddShardBlockHeader,
		},
		Forwarder: sc.routeFrame,
		Logger:    cfg.Logger,
	})
	return sc, nil
}

// RemoteChainID returns an independent copy of the expected remote chain ID.
func (sc *SlaveConn) RemoteChainID() *big.Int {
	return new(big.Int).Set(sc.remoteChainID)
}

// SendPing verifies that the slave's PONG matches its configured chain ID.
// The first PING checks identity without a root tip. After all slaves connect
// to each other, a second PING carries the root tip to initialize their shards.
// A nil rootTip sends an identity-only PING.
// The caller is responsible for setting an appropriate context deadline.
// The owner must close the connection on any error and complete identity
// verification before starting business activity.
func (sc *SlaveConn) SendPing(ctx context.Context, rootTip *qkctypes.RootBlock) error {
	req := &wire.PingRequest{}
	if rootTip != nil {
		encoded, err := rootTip.MarshalSSZ()
		if err != nil {
			return fmt.Errorf("encode ping root tip: %w", err)
		}
		req.HasRootTip, req.RootTip = true, encoded
	}
	resp, err := sc.SendRPC(ctx, byte(wire.ClusterOpPing), req)
	if err != nil {
		return err
	}
	pong, ok := resp.(*wire.PongResponse)
	if !ok {
		return fmt.Errorf("unexpected ping response %T", resp)
	}
	id := wire.ChainIDFromSSZ(pong.ChainID)
	if id.Cmp(sc.remoteChainID) != 0 {
		return fmt.Errorf("slave chain ID mismatch: expected %s, got %s", sc.remoteChainID, id)
	}
	return nil
}

// SendConnectToSlaves asks the slave to connect to the listed slaves. As in
// pyquarkchain, success requires one empty result per requested slave. The owner
// must call this on one slave at a time and await its response before sending
// to the next slave; failure aborts cluster initialization without a retry.
// The caller is responsible for setting an appropriate context deadline.
func (sc *SlaveConn) SendConnectToSlaves(ctx context.Context, slaves []comm.SlaveInfo) error {
	req := &wire.ConnectToSlavesRequest{SlaveInfoList: make([]wire.SlaveInfo, len(slaves))}
	for i, slave := range slaves {
		if slave.Host == "" || slave.Port == 0 {
			return fmt.Errorf("invalid slave address at index %d: host %q, port %d", i, slave.Host, slave.Port)
		}
		if slave.ChainID == nil || slave.ChainID.Sign() <= 0 {
			return fmt.Errorf("invalid slave chain ID at index %d: %v", i, slave.ChainID)
		}
		id, err := wire.ChainIDToSSZ(slave.ChainID)
		if err != nil {
			return fmt.Errorf("encode slave chain ID at index %d: %w", i, err)
		}
		req.SlaveInfoList[i] = wire.SlaveInfo{Host: []byte(slave.Host), Port: slave.Port, ChainID: id}
	}
	resp, err := sc.SendRPC(ctx, byte(wire.ClusterOpConnectToSlavesRequest), req)
	if err != nil {
		return err
	}
	result, ok := resp.(*wire.ConnectToSlavesResponse)
	if !ok {
		return fmt.Errorf("unexpected connect to slaves response %T", resp)
	}
	if len(result.ResultList) != len(req.SlaveInfoList) {
		return fmt.Errorf("slave %s returned %d connection results, want %d", sc.remoteChainID, len(result.ResultList), len(req.SlaveInfoList))
	}
	for index, failure := range result.ResultList {
		if len(failure) != 0 {
			target := wire.ChainIDFromSSZ(req.SlaveInfoList[index].ChainID)
			return fmt.Errorf("slave %s failed to connect to slave %s: %q", sc.remoteChainID, target, failure)
		}
	}
	return nil
}

// SendAddRootBlock encodes and sends a root block. A nonzero remote
// ErrorCode is returned as *comm.RemoteError; transport and context errors are
// returned unchanged.
func (sc *SlaveConn) SendAddRootBlock(ctx context.Context, root *qkctypes.RootBlock) error {
	if root == nil {
		return errors.New("root block must not be nil")
	}
	encoded, err := root.MarshalSSZ()
	if err != nil {
		return fmt.Errorf("encode root block: %w", err)
	}
	req := &wire.AddRootBlockRequest{RootBlock: encoded}
	resp, err := sc.SendRPC(ctx, byte(wire.ClusterOpAddRootBlockRequest), req)
	if err != nil {
		return err
	}
	result, ok := resp.(*wire.AddRootBlockResponse)
	if !ok {
		return fmt.Errorf("unexpected add root block response %T", resp)
	}
	if result.ErrorCode != 0 {
		return &comm.RemoteError{Code: result.ErrorCode}
	}
	return nil
}

// SendGetRootChainStakes returns the slave's staking response. A nonzero remote
// ErrorCode is returned as *comm.RemoteError; transport and context errors are
// returned unchanged. Business values are decoded before returning.
func (sc *SlaveConn) SendGetRootChainStakes(ctx context.Context, address common.Address, shardBlockHash common.Hash) (*comm.RootChainStakesResult, error) {
	req := &wire.GetRootChainStakesRequest{Address: address, ShardBlockHash: shardBlockHash}
	resp, err := sc.SendRPC(ctx, byte(wire.ClusterOpGetRootChainStakesRequest), req)
	if err != nil {
		return nil, err
	}
	result, ok := resp.(*wire.GetRootChainStakesResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected get root chain stakes response %T", resp)
	}
	if result.ErrorCode != 0 {
		return nil, &comm.RemoteError{Code: result.ErrorCode}
	}
	stakes := new(uint256.Int)
	if err := stakes.UnmarshalSSZ(result.Stakes[:]); err != nil {
		return nil, fmt.Errorf("decode root chain stakes: %w", err)
	}
	return &comm.RootChainStakesResult{Stakes: stakes, Signer: result.Signer}, nil
}

// SendGetUnconfirmedHeaders asks a slave for its unconfirmed shard headers.
func (sc *SlaveConn) SendGetUnconfirmedHeaders(ctx context.Context) ([]*types.Header, error) {
	resp, err := sc.SendRPC(ctx, byte(wire.ClusterOpGetUnconfirmedHeadersRequest), &wire.GetUnconfirmedHeadersRequest{})
	if err != nil {
		return nil, err
	}
	result, ok := resp.(*wire.GetUnconfirmedHeadersResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected get unconfirmed headers response %T", resp)
	}
	if result.ErrorCode != 0 {
		return nil, &comm.RemoteError{Code: result.ErrorCode}
	}
	headers := make([]*types.Header, len(result.HeaderList))
	for i, encoded := range result.HeaderList {
		headers[i] = new(types.Header)
		if err := rlp.DecodeBytes(encoded, headers[i]); err != nil {
			return nil, fmt.Errorf("decode unconfirmed header %d: %w", i, err)
		}
	}
	return headers, nil
}

// routeFrame consumes peer traffic so it cannot enter local RPC dispatch.
func (sc *SlaveConn) routeFrame(frame *wire.ClusterFrame) bool {
	if frame.ClusterPeerID == 0 {
		return false
	}

	// TODO: Forward nonzero ClusterPeerID frames when peer routing is implemented.
	return true
}

// ── topology & shard activation handlers ───────────────────────────────────

func (sc *SlaveConn) handleAddShardBlockHeader(req wire.SSZMessage) (wire.SSZMessage, error) {
	request := req.(*wire.AddShardBlockHeaderRequest)
	header := new(types.Header)
	if err := rlp.DecodeBytes(request.ShardBlockHeader, header); err != nil {
		return nil, fmt.Errorf("decode shard block header: %w", err)
	}
	amount := new(uint256.Int)
	if err := amount.UnmarshalSSZ(request.CoinbaseAmount[:]); err != nil {
		return nil, fmt.Errorf("decode coinbase amount: %w", err)
	}
	code, err := wire.ErrorCode(sc.handler.AddShardBlockHeader(sc.RemoteChainID(), header, amount))
	if err != nil {
		return nil, err
	}
	return &wire.AddShardBlockHeaderResponse{ErrorCode: code}, nil
}
