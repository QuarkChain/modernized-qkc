package p2pnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/qkc/net/conn"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

type receivedFrame struct {
	chainID *big.Int
	frame   *wire.ClusterFrame
}

func (r *receivedFrame) check(t *testing.T, peerID uint64, frame *wire.P2PFrame) {
	t.Helper()
	if r.frame.ClusterPeerID != peerID || r.chainID.Cmp(frame.ChainID) != 0 ||
		r.frame.Opcode != frame.Opcode || r.frame.RPCID != frame.RPCID || !bytes.Equal(r.frame.Payload, frame.Payload) {
		t.Fatalf("received chain %s frame %+v, want peer %d frame %+v", r.chainID, r.frame, peerID, frame)
	}
}

type peerTestHandler struct {
	newTip          func(uint64, *qkctypes.RootBlockHeader) error
	newTransactions func(uint64, types.Transactions) error
	getRootHeaders  func(uint64, common.Hash, *big.Int, uint32, uint32, bool) (*RootHeadersResult, error)
	getRootBlocks   func(uint64, []common.Hash) ([]*qkctypes.RootBlock, error)
}

func (handler *peerTestHandler) NewTip(peerID uint64, tip *qkctypes.RootBlockHeader) error {
	if handler.newTip == nil {
		return errors.New("unexpected root tip")
	}
	return handler.newTip(peerID, tip)
}

func (handler *peerTestHandler) NewTransactions(peerID uint64, transactions types.Transactions) error {
	if handler.newTransactions == nil {
		return errors.New("unexpected transactions")
	}
	return handler.newTransactions(peerID, transactions)
}

func (handler *peerTestHandler) GetRootHeaders(peerID uint64, hash common.Hash, height *big.Int, limit, skip uint32, reverse bool) (*RootHeadersResult, error) {
	if handler.getRootHeaders == nil {
		return nil, errors.New("unexpected root headers request")
	}
	return handler.getRootHeaders(peerID, hash, height, limit, skip, reverse)
}

func (handler *peerTestHandler) GetRootBlocks(peerID uint64, hashes []common.Hash) ([]*qkctypes.RootBlock, error) {
	if handler.getRootBlocks == nil {
		return nil, errors.New("unexpected root blocks request")
	}
	return handler.getRootBlocks(peerID, hashes)
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
		var zero T
		return zero
	}
}

func testPeerConfig(rw *p2p.MsgPipeRW, id uint64) PeerConnConfig {
	return PeerConnConfig{
		Remote:        p2p.NewPeerPipe(enode.ID{31: byte(id)}, "test", nil, rw),
		ReadWriter:    rw,
		ClusterPeerID: id,
		Handler:       &peerTestHandler{},
		Forwarder:     func(*big.Int, *wire.ClusterFrame) error { return errors.New("unexpected shard frame") },
	}
}

func newTestPeer(t *testing.T, cfg PeerConnConfig) *PeerConn {
	t.Helper()
	peer, err := NewPeerConn(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		peer.Close()
		peer.WaitHandlers()
	})
	return peer
}

func rawPeer(t *testing.T) (*PeerConn, *p2p.MsgPipeRW) {
	t.Helper()
	local, remote := p2p.MsgPipe()
	t.Cleanup(func() { remote.Close() })
	cfg := testPeerConfig(local, 3)
	cfg.Forwarder = func(_ *big.Int, frame *wire.ClusterFrame) error {
		return fmt.Errorf("forwarded unexpected opcode 0x%x", frame.Opcode)
	}
	return newTestPeer(t, cfg), remote
}

func sendRaw(test *testing.T, remote p2p.MsgWriter, opcode byte, rpcID uint64, message wire.SSZMessage) {
	sendRawToChain(test, remote, wire.RootChainID(), opcode, rpcID, message)
}

func sendRawToChain(test *testing.T, remote p2p.MsgWriter, chainID *big.Int, opcode byte, rpcID uint64, message wire.SSZMessage) {
	test.Helper()
	payload, err := message.MarshalSSZ()
	if err != nil {
		test.Fatal(err)
	}
	data, err := wire.EncodeP2PFrame(&wire.P2PFrame{ChainID: chainID, Opcode: opcode, RPCID: rpcID, Payload: payload})
	if err != nil {
		test.Fatal(err)
	}
	if err := remote.WriteMsg(p2p.Msg{Code: frameCode, Size: uint32(len(data)), Payload: bytes.NewReader(data)}); err != nil {
		test.Fatal(err)
	}
}

func TestPeerConnConstructionAndIdentity(t *testing.T) {
	local, remote := p2p.MsgPipe()
	defer remote.Close()
	cfg := testPeerConfig(local, 42)
	peer := newTestPeer(t, cfg)
	if peer.ID() != cfg.Remote.ID() || peer.ClusterPeerID() != cfg.ClusterPeerID {
		t.Fatal("incorrect peer identity")
	}
	frame := &wire.P2PFrame{ChainID: wire.RootChainID()}
	if err := peer.WriteFrame(frame); !errors.Is(err, conn.ErrNotActive) {
		t.Fatal("send before Start:", err)
	}
	for _, test := range []struct {
		name string
		edit func(*PeerConnConfig)
	}{
		{"remote", func(cfg *PeerConnConfig) { cfg.Remote = nil }},
		{"writer", func(cfg *PeerConnConfig) { cfg.ReadWriter = nil }},
		{"reserved ID", func(cfg *PeerConnConfig) { cfg.ClusterPeerID = 0 }},
		{"forwarder", func(cfg *PeerConnConfig) { cfg.Forwarder = nil }},
		{"handler", func(cfg *PeerConnConfig) { cfg.Handler = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := cfg
			test.edit(&invalid)
			if _, err := NewPeerConn(invalid); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	peer.Close()
	peer.Start()
	if err := peer.WriteFrame(frame); !errors.Is(err, conn.ErrConnectionClosed) {
		t.Fatal("send after Close:", err)
	}
}

func TestPeerConnForwardsRawFrames(t *testing.T) {
	firstRW, secondRW := p2p.MsgPipe()
	defer firstRW.Close()
	first := newTestPeer(t, testPeerConfig(firstRW, 1))
	secondCfg := testPeerConfig(secondRW, 2)
	frames := make(chan receivedFrame, 2)
	secondCfg.Forwarder = func(chainID *big.Int, frame *wire.ClusterFrame) error {
		frames <- receivedFrame{chainID, frame}
		return nil
	}
	second := newTestPeer(t, secondCfg)
	first.Start()
	second.Start()

	for _, frame := range []*wire.P2PFrame{
		{ChainID: big.NewInt(3), Opcode: 0x42, Payload: []byte{0xfa}},
		{ChainID: new(big.Int).Lsh(big.NewInt(1), 200), Opcode: byte(wire.CommandOpGetShardHeadersRequest), RPCID: 19, Payload: []byte{0xff, 0x00}},
		{ChainID: big.NewInt(3), Opcode: byte(wire.CommandOpGetShardHeadersResponse), RPCID: 19, Payload: []byte{0xff}},
		{ChainID: big.NewInt(4), Opcode: byte(wire.CommandOpGetShardBlocksRequest), RPCID: 1, Payload: []byte{0xff}},
		{ChainID: big.NewInt(4), Opcode: byte(wire.CommandOpGetShardBlocksResponse), RPCID: 1, Payload: []byte{0xff}},
		{ChainID: big.NewInt(3), Opcode: byte(wire.CommandOpNewTip), Payload: []byte{0xff}},
		{ChainID: big.NewInt(3), Opcode: byte(wire.CommandOpNewTransactions), Payload: []byte{0xff}},
		{ChainID: big.NewInt(3), Opcode: byte(wire.CommandOpGetRootHeadersRequest), RPCID: 1, Payload: []byte{0xff}},
	} {
		if err := first.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		received := await(t, frames)
		received.check(t, second.ClusterPeerID(), frame)
	}
}

func TestPeerConnForwarderErrorClosesConnection(t *testing.T) {
	firstRW, secondRW := p2p.MsgPipe()
	defer firstRW.Close()
	first := newTestPeer(t, testPeerConfig(firstRW, 1))
	secondCfg := testPeerConfig(secondRW, 2)
	secondCfg.Forwarder = func(*big.Int, *wire.ClusterFrame) error {
		return errors.New("forwarding failed")
	}
	second := newTestPeer(t, secondCfg)
	first.Start()
	second.Start()

	if err := first.WriteFrame(&wire.P2PFrame{ChainID: big.NewInt(3), Opcode: 0x42}); err != nil {
		t.Fatal(err)
	}
	await(t, second.WaitUntilClosed())
	await(t, first.WaitUntilClosed())
}

func TestPeerConnRejectsOversizedFrame(t *testing.T) {
	local, remote := p2p.MsgPipe()
	defer remote.Close()
	cfg := testPeerConfig(local, 42)
	frames := make(chan *wire.ClusterFrame, 1)
	cfg.Forwarder = func(_ *big.Int, frame *wire.ClusterFrame) error {
		frames <- frame
		return nil
	}
	peer := newTestPeer(t, cfg)
	peer.Start()
	payload := make([]byte, maxP2PFrameSize+1)
	if err := remote.WriteMsg(p2p.Msg{Code: frameCode, Size: uint32(len(payload)), Payload: bytes.NewReader(payload)}); err != nil {
		t.Fatal(err)
	}
	await(t, peer.WaitUntilClosed())
	select {
	case frame := <-frames:
		t.Fatalf("unexpected forwarded frame %+v", frame)
	default:
	}
}

func TestPeerRootRPC(t *testing.T) {
	first, remote := rawPeer(t)
	header := &qkctypes.RootBlockHeader{Number: 20}
	block := qkctypes.NewRootBlockWithHeader(header)
	height := new(big.Int).Lsh(big.NewInt(1), 200)
	hash := common.Hash{1}
	cfg := testPeerConfig(remote, 2)
	cfg.Handler = &peerTestHandler{
		getRootHeaders: func(peerID uint64, originHash common.Hash, originHeight *big.Int, limit, skip uint32, reverse bool) (*RootHeadersResult, error) {
			validOrigin := originHeight == nil && originHash == hash && reverse
			if originHeight != nil {
				validOrigin = originHeight.Cmp(height) == 0 && originHash == (common.Hash{}) && !reverse
			}
			if peerID != 2 || !validOrigin || limit != 3 || skip != 4 {
				return nil, errors.New("incorrect decoded root headers request")
			}
			return &RootHeadersResult{RootTip: header, Headers: []*qkctypes.RootBlockHeader{header}}, nil
		},
		getRootBlocks: func(peerID uint64, hashes []common.Hash) ([]*qkctypes.RootBlock, error) {
			if peerID != 2 || len(hashes) != 1 || hashes[0] != block.Hash() {
				return nil, errors.New("incorrect decoded root blocks request")
			}
			return []*qkctypes.RootBlock{block}, nil
		},
	}
	second := newTestPeer(t, cfg)
	first.Start()
	second.Start()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	for _, query := range []struct {
		hash    common.Hash
		height  *big.Int
		reverse bool
	}{
		{hash: hash, reverse: true},
		{height: height},
	} {
		result, err := first.SendGetRootHeaders(ctx, query.hash, query.height, 3, 4, query.reverse)
		if err != nil {
			t.Fatal(err)
		}
		if result.RootTip.Hash() != header.Hash() || len(result.Headers) != 1 || result.Headers[0].Hash() != header.Hash() {
			t.Fatal("incorrect decoded root headers")
		}
	}
	blocks, err := first.SendGetRootBlocks(ctx, []common.Hash{block.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Hash() != block.Hash() {
		t.Fatal("incorrect decoded root blocks")
	}
}

func TestPeerRootCommands(t *testing.T) {
	first, remote := rawPeer(t)
	chainID := new(big.Int).Lsh(big.NewInt(1), 200)
	transactions := types.Transactions{types.NewTx(&types.DynamicFeeTx{ChainID: chainID, Nonce: 5, Gas: 21000})}
	rootTip := &qkctypes.RootBlockHeader{Number: 21}
	calls := make(chan error, 1)
	cfg := testPeerConfig(remote, 2)
	cfg.Handler = &peerTestHandler{
		newTip: func(peerID uint64, tip *qkctypes.RootBlockHeader) error {
			var err error
			if peerID != 2 || tip.Hash() != rootTip.Hash() {
				err = errors.New("incorrect decoded root tip")
			}
			calls <- err
			return err
		},
		newTransactions: func(peerID uint64, decoded types.Transactions) error {
			var err error
			if peerID != 2 || len(decoded) != 1 || decoded[0].Hash() != transactions[0].Hash() || decoded[0].ChainId().Cmp(chainID) != 0 {
				err = errors.New("incorrect decoded transactions")
			}
			calls <- err
			return err
		},
	}
	second := newTestPeer(t, cfg)
	first.Start()
	second.Start()
	for _, send := range []func() error{
		func() error { return first.SendNewTip(rootTip) },
		func() error { return first.SendNewTransactions(transactions) },
	} {
		if err := send(); err != nil {
			t.Fatal(err)
		}
		if err := await(t, calls); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeerShardResponseDoesNotConsumeRootRPC(t *testing.T) {
	local, remote := p2p.MsgPipe()
	defer remote.Close()
	cfg := testPeerConfig(local, 42)
	frames := make(chan receivedFrame, 1)
	cfg.Forwarder = func(chainID *big.Int, frame *wire.ClusterFrame) error {
		frames <- receivedFrame{chainID, frame}
		return nil
	}
	peer := newTestPeer(t, cfg)
	peer.Start()
	results := make(chan error, 1)
	go func() {
		_, err := peer.SendGetRootHeaders(t.Context(), common.Hash{1}, nil, 1, 0, true)
		results <- err
	}()
	value, err := (&Transport{rw: remote}).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	request := value.(*wire.P2PFrame)
	for _, opcode := range []wire.CommandOp{wire.CommandOpGetRootHeadersResponse, wire.CommandOpGetShardHeadersResponse} {
		frame := &wire.P2PFrame{ChainID: big.NewInt(3), Opcode: byte(opcode), RPCID: request.RPCID, Payload: []byte{0xff}}
		if err := (&Transport{rw: remote}).WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		received := await(t, frames)
		received.check(t, 42, frame)
	}
	select {
	case err := <-results:
		t.Fatalf("shard response completed root RPC: %v", err)
	default:
	}
	header, err := new(qkctypes.RootBlockHeader).MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	sendRaw(t, remote, byte(wire.CommandOpGetRootHeadersResponse), request.RPCID, &wire.GetRootHeadersResponse{RootTip: header})
	if err := await(t, results); err != nil {
		t.Fatal(err)
	}
}

func TestPeerRootOperationsRejectInvalidArguments(t *testing.T) {
	peer, _ := rawPeer(t)
	peer.Start()
	for _, send := range []func() error{
		func() error { return peer.SendNewTip(nil) },
		func() error { return peer.SendNewTransactions(types.Transactions{nil}) },
		func() error {
			_, err := peer.SendGetRootHeaders(t.Context(), common.Hash{1}, big.NewInt(1), 1, 0, true)
			return err
		},
		func() error {
			_, err := peer.SendGetRootHeaders(t.Context(), common.Hash{}, big.NewInt(-1), 1, 0, true)
			return err
		},
		func() error {
			_, err := peer.SendGetRootHeaders(t.Context(), common.Hash{}, new(big.Int).Lsh(big.NewInt(1), 256), 1, 0, true)
			return err
		},
	} {
		if err := send(); err == nil || !peer.IsActive() {
			t.Fatalf("invalid argument: error = %v, active = %v", err, peer.IsActive())
		}
	}
}

func TestPeerRootRejectsInvalidTraffic(t *testing.T) {
	header, err := new(qkctypes.RootBlockHeader).MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name         string
		opcode       wire.CommandOp
		rpcID        uint64
		message      wire.SSZMessage
		handlerCalls int
	}{
		{"unknown opcode", 0x42, 0, nil, 0},
		{"hello after handshake", wire.CommandOpHello, 0, &wire.HelloCommand{}, 0},
		{"shard request on root", wire.CommandOpGetShardHeadersRequest, 1, nil, 0},
		{"malformed SSZ", wire.CommandOpNewTip, 0, nil, 0},
		{"malformed root tip", wire.CommandOpNewTip, 0, &wire.NewTipCommand{RootTip: []byte{0xff}}, 0},
		{"shard tip on root", wire.CommandOpNewTip, 0, &wire.NewTipCommand{RootTip: header, ShardTip: []byte{1}}, 0},
		{"malformed transactions", wire.CommandOpNewTransactions, 0, &wire.NewTransactionsCommand{Transactions: []byte{0xff}}, 0},
		{"invalid origin type", wire.CommandOpGetRootHeadersRequest, 1, &wire.GetRootHeadersRequest{Type: 2}, 0},
		{"invalid direction", wire.CommandOpGetRootHeadersRequest, 1, &wire.GetRootHeadersRequest{Direction: 2}, 0},
		{"nonzero command ID", wire.CommandOpNewTip, 1, &wire.NewTipCommand{RootTip: header}, 0},
		{"zero request ID", wire.CommandOpGetRootHeadersRequest, 0, &wire.GetRootHeadersRequest{}, 0},
		{"business failure", wire.CommandOpNewTip, 0, &wire.NewTipCommand{RootTip: header}, 1},
	} {
		t.Run(entry.name, func(t *testing.T) {
			local, remote := p2p.MsgPipe()
			defer remote.Close()
			calls := make(chan struct{}, 1)
			cfg := testPeerConfig(local, 42)
			cfg.Handler = &peerTestHandler{
				newTip: func(uint64, *qkctypes.RootBlockHeader) error {
					calls <- struct{}{}
					return errors.New("business failure")
				},
				newTransactions: func(uint64, types.Transactions) error {
					calls <- struct{}{}
					return errors.New("business failure")
				},
				getRootHeaders: func(uint64, common.Hash, *big.Int, uint32, uint32, bool) (*RootHeadersResult, error) {
					calls <- struct{}{}
					return nil, errors.New("business failure")
				},
			}
			forwarded := make(chan struct{}, 1)
			cfg.Forwarder = func(*big.Int, *wire.ClusterFrame) error {
				forwarded <- struct{}{}
				return errors.New("unexpected root forwarding")
			}
			peer := newTestPeer(t, cfg)
			peer.Start()
			payload := []byte{0xff}
			if entry.message != nil {
				payload, err = entry.message.MarshalSSZ()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := (&Transport{rw: remote}).WriteFrame(&wire.P2PFrame{
				ChainID: wire.RootChainID(), Opcode: byte(entry.opcode), RPCID: entry.rpcID, Payload: payload,
			}); err != nil {
				t.Fatal(err)
			}
			await(t, peer.WaitUntilClosed())
			peer.WaitHandlers()
			if len(calls) != entry.handlerCalls || len(forwarded) != 0 {
				t.Fatalf("handler calls = %d, want %d; forwarded = %d", len(calls), entry.handlerCalls, len(forwarded))
			}
		})
	}
}

func TestRPCCancellationAndLateResponses(test *testing.T) {
	peer, remote := rawPeer(test)
	peer.Start()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := peer.SendGetRootHeaders(ctx, common.Hash{1}, nil, 1, 0, true)
		result <- err
	}()
	frame, err := (&Transport{rw: remote}).ReadFrame()
	if err != nil {
		test.Fatal(err)
	}
	request := frame.(*wire.P2PFrame)
	cancel()
	if err := await(test, result); !errors.Is(err, context.Canceled) {
		test.Fatal("RPC cancellation:", err)
	}
	for range 2 {
		sendRaw(test, remote, byte(wire.CommandOpGetRootHeadersResponse), request.RPCID, &wire.GetRootHeadersResponse{})
	}
	if _, err := peer.SendGetRootHeaders(ctx, common.Hash{1}, nil, 1, 0, true); !errors.Is(err, context.Canceled) {
		test.Fatal("cancelled RPC:", err)
	}
	if !peer.IsActive() {
		test.Fatal("late responses closed connection")
	}
}

func TestCloseWakesRPC(test *testing.T) {
	peer, remote := rawPeer(test)
	peer.Start()
	result := make(chan error, 1)
	go func() {
		_, err := peer.SendGetRootHeaders(context.Background(), common.Hash{1}, nil, 1, 0, true)
		result <- err
	}()
	if _, err := (&Transport{rw: remote}).ReadFrame(); err != nil {
		test.Fatal(err)
	}
	peer.Close()
	if err := await(test, result); !errors.Is(err, conn.ErrConnectionClosed) {
		test.Fatal("closed pending RPC:", err)
	}
}

func TestTransportTruncatedPayload(t *testing.T) {
	for _, entry := range []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"partial", []byte{0}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			local, remote := p2p.MsgPipe()
			defer local.Close()
			defer remote.Close()
			written := make(chan error, 1)
			go func() {
				written <- remote.WriteMsg(p2p.Msg{
					Code: frameCode, Size: p2pFrameHeaderSize, Payload: bytes.NewReader(entry.payload),
				})
			}()
			if _, err := (&Transport{rw: local}).ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated payload error = %v, want unexpected EOF", err)
			}
			if err := await(t, written); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGetHTransportRoundTrip(t *testing.T) {
	peers := make(chan *PeerConn, 2)
	frames := make(chan receivedFrame, 2)
	var stacks []*node.Node
	for index := range 2 {
		network := p2p.Config{ListenAddr: "127.0.0.1:0", MaxPeers: 2, NoDiscovery: true}
		if index == 1 {
			network.StaticNodes = []*enode.Node{stacks[0].Server().Self()}
		}
		stack, err := node.New(&node.Config{Name: fmt.Sprint("peer", index), P2P: network})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stack.Close() })
		stack.RegisterProtocols([]p2p.Protocol{{Name: "qkc", Version: 1, Length: 1, Run: func(remote *p2p.Peer, rw p2p.MsgReadWriter) error {
			peer, err := NewPeerConn(PeerConnConfig{
				Remote:        remote,
				ReadWriter:    rw,
				ClusterPeerID: uint64(index + 1),
				Handler:       &peerTestHandler{},
				Forwarder: func(chainID *big.Int, frame *wire.ClusterFrame) error {
					frames <- receivedFrame{chainID, frame}
					return nil
				},
			})
			if err != nil {
				return err
			}
			defer peer.Close()
			peer.Start()
			peers <- peer
			<-peer.WaitUntilClosed()
			return nil
		}}})
		if err := stack.Start(); err != nil {
			t.Fatal(err)
		}
		stacks = append(stacks, stack)
	}
	first, second := await(t, peers), await(t, peers)
	frame := &wire.P2PFrame{ChainID: big.NewInt(10001), Opcode: 0x42, RPCID: 19, Payload: []byte{0xff, 0x00}}
	if err := first.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	received := await(t, frames)
	received.check(t, second.ClusterPeerID(), frame)
	first.Close()
	await(t, second.WaitUntilClosed())
}
