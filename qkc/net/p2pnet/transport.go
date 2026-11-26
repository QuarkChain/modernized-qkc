package p2pnet

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/qkc/net/conn"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
)

const (
	frameCode              = 0
	p2pFrameHeaderSize     = 32 + 1 + 8     // chainId + opcode + rpcId
	maxP2PFrameSize        = (16 << 20) - 1 // geth frame max size
	maxP2PFramePayloadSize = maxP2PFrameSize - p2pFrameHeaderSize
)

var _ conn.FrameTransport = (*Transport)(nil)

type Transport struct {
	remote *p2p.Peer
	rw     p2p.MsgReadWriter
}

func NewTransport(remote *p2p.Peer, rw p2p.MsgReadWriter) (*Transport, error) {
	if remote == nil || rw == nil {
		return nil, errors.New("missing geth peer or message stream")
	}
	return &Transport{remote: remote, rw: rw}, nil
}

func (transport *Transport) RemoteAddr() string { return transport.remote.RemoteAddr().String() }

func (transport *Transport) ReadFrame() (wire.Frame, error) {
	message, err := transport.rw.ReadMsg()
	if err != nil {
		return nil, err
	}
	if message.Code != frameCode {
		return nil, fmt.Errorf("unexpected P2P message code %d", message.Code)
	}
	if message.Size > maxP2PFrameSize {
		return nil, fmt.Errorf("P2P frame too large: %d bytes", message.Size)
	}
	data := make([]byte, message.Size)
	if _, err := io.ReadFull(message.Payload, data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("reading P2P message payload (size=%d): %w", message.Size, err)
	}
	return wire.DecodeP2PFrame(data)
}

func (transport *Transport) WriteFrame(value wire.Frame) error {
	frame, ok := value.(*wire.P2PFrame)
	if !ok || frame == nil {
		return fmt.Errorf("%w: expected P2P frame, got %T", wire.ErrInvalidFrame, value)
	}
	if len(frame.Payload) > maxP2PFramePayloadSize {
		return fmt.Errorf("%w: payload too large", wire.ErrInvalidFrame)
	}
	data, err := wire.EncodeP2PFrame(frame)
	if err != nil {
		return err
	}
	return transport.rw.WriteMsg(p2p.Msg{
		Code: frameCode, Size: uint32(len(data)), Payload: bytes.NewReader(data),
	})
}

func (transport *Transport) Close() error {
	transport.remote.Disconnect(p2p.DiscRequested)
	return nil
}
