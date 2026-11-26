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

// Package wire defines frame codecs for internal cluster and external peer
// communication.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
)

// RootChainID returns a fresh zero ID for root-chain traffic handled by the
// Master. Zero is never a valid shard chain ID.
func RootChainID() *big.Int {
	return new(big.Int)
}

const (
	chainIDSize       = 32
	clusterPeerIDSize = 8
	opcodeSize        = 1
	rpcIDSize         = 8
	frameHeader       = 4 // internal TCP payload length prefix
	frameBodyHeader   = opcodeSize + rpcIDSize

	// maxClusterPayloadLen is the algorithmic capacity bound even when no
	// configurable payload limit is set. Go slice lengths use int, so reserve
	// the largest cluster frame header within the 32-bit int maximum to keep
	// length arithmetic valid and the bound identical on 32-bit and 64-bit platforms.
	maxClusterPayloadLen = math.MaxInt32 - frameHeader - clusterPeerIDSize - frameBodyHeader
)

// QKC sends P2PFrame through geth's P2P protocol, which requires
// a message code and a payload. Since P2PFrame already contains its
// own opcode, QKC sets the message code to 0 and uses the encoded
// P2PFrame as the message payload.
//
// Geth adds the protocol offset to the message code and encodes
// the RLPx frame body as:
//
//	without Snappy: [RLP(code)][P2PFrame]
//	with Snappy:    [RLP(code)][Snappy(P2PFrame)]
//
// The combined size of the encoded code and payload must fit within
// the RLPx 24-bit frame size limit. Therefore, the maximum P2PFrame
// size must account for both the encoded code and possible Snappy
// expansion.
const (
	// rlpxMaxFrameSize bounds the combined code and message bytes shown above.
	rlpxMaxFrameSize = (1 << 24) - 1
	// rlpxMaxCodeSize reserves at most 9 bytes for RLP(code): a prefix
	// followed by 8 value bytes. It does not include the QKC opcode.
	rlpxMaxCodeSize = 9
	// Snappy can expand data. Its encoded size is bounded by 32 + n + n/6
	// for n input bytes; snappyOverhead is the fixed 32-byte term.
	snappyOverhead = 32

	// MaxP2PFrameSize bounds the encoded P2PFrame length before compression,
	// including the QKC header. For n bytes, both transport layouts must fit:
	//
	//	without Snappy: 9 + n <= rlpxMaxFrameSize
	//	with Snappy:    9 + 32 + n + n/6 <= rlpxMaxFrameSize
	//
	// The stricter Snappy bound gives the conservative 6/7 factor below.
	MaxP2PFrameSize = (rlpxMaxFrameSize - rlpxMaxCodeSize - snappyOverhead) * 6 / 7

	// P2PFrameHeaderSize is the 41-byte QKC frame header.
	P2PFrameHeaderSize = chainIDSize + frameBodyHeader
	// MaxP2PPayloadLen bounds P2PFrame.Payload after subtracting the QKC header.
	// External P2P frames and future virtual peer connections share this limit.
	// ClusterPeerID is internal routing metadata and is not sent in P2PFrame.
	MaxP2PPayloadLen = MaxP2PFrameSize - P2PFrameHeaderSize
)

// Frame exposes the RPC body while preserving transport-specific metadata.
// WithBody returns a new frame without modifying the receiver.
type Frame interface {
	GetOpcode() byte
	GetRPCID() uint64
	GetPayload() []byte
	WithBody(opcode byte, rpcID uint64, payload []byte) Frame
}

// ClusterFrame carries one internal TCP message between cluster processes.
//
// Wire layout:
//
//	[4B payload_len][8B cluster_peer_id, Master↔slave only][1B opcode][8B rpc_id][payload]
//
// payload_len counts only payload. ClusterPeerID routes traffic through a
// virtual peer connection on Master↔slave links; it is zero for ordinary
// cluster traffic and is omitted entirely on direct slave↔slave links.
//
// The codec selected for the connection determines whether ClusterPeerID is
// present on the wire.
type ClusterFrame struct {
	ClusterPeerID uint64
	Opcode        byte
	RPCID         uint64 // Request ID; responses echo it. Zero means non-RPC.
	Payload       []byte
}

func (frame *ClusterFrame) GetOpcode() byte    { return frame.Opcode }
func (frame *ClusterFrame) GetRPCID() uint64   { return frame.RPCID }
func (frame *ClusterFrame) GetPayload() []byte { return frame.Payload }
func (frame *ClusterFrame) WithBody(opcode byte, rpcID uint64, payload []byte) Frame {
	return &ClusterFrame{ClusterPeerID: frame.ClusterPeerID, Opcode: opcode, RPCID: rpcID, Payload: payload}
}

// P2PFrame is an external cluster↔cluster message carried as one devp2p
// protocol message. The devp2p message boundary supplies the frame length.
//
// Wire payload: [32B chain_id][1B opcode][8B rpc_id][payload].
type P2PFrame struct {
	// ChainID selects the destination chain. RootChainID routes root-chain
	// traffic to the Master.
	ChainID *big.Int
	Opcode  byte
	RPCID   uint64
	Payload []byte
}

func (frame *P2PFrame) GetOpcode() byte    { return frame.Opcode }
func (frame *P2PFrame) GetRPCID() uint64   { return frame.RPCID }
func (frame *P2PFrame) GetPayload() []byte { return frame.Payload }
func (frame *P2PFrame) WithBody(opcode byte, rpcID uint64, payload []byte) Frame {
	var chainID *big.Int
	if frame.ChainID != nil {
		chainID = new(big.Int).Set(frame.ChainID)
	}
	return &P2PFrame{ChainID: chainID, Opcode: opcode, RPCID: rpcID, Payload: payload}
}

// ReadClusterFrame reads one direct slave↔slave internal TCP frame without
// a ClusterPeerID or a configurable payload limit.
func ReadClusterFrame(r io.Reader) (*ClusterFrame, error) {
	return readClusterFrame(r, false, 0)
}

// WriteClusterFrame writes one direct slave↔slave internal TCP frame without
// a ClusterPeerID or a configurable payload limit.
func WriteClusterFrame(w io.Writer, frame Frame) error {
	return writeClusterFrame(w, frame, false, 0)
}

// ReadClusterFrameWithPeerID reads one Master↔slave internal TCP frame.
// maxPayloadLen is zero for ordinary cluster traffic or at most MaxP2PPayloadLen
// otherwise. Future virtual peer connections must use MaxP2PPayloadLen.
func ReadClusterFrameWithPeerID(r io.Reader, maxPayloadLen uint32) (*ClusterFrame, error) {
	return readClusterFrame(r, true, maxPayloadLen)
}

// WriteClusterFrameWithPeerID writes one Master↔slave internal TCP frame.
// maxPayloadLen follows the same rules as ReadClusterFrameWithPeerID.
func WriteClusterFrameWithPeerID(w io.Writer, frame Frame, maxPayloadLen uint32) error {
	return writeClusterFrame(w, frame, true, maxPayloadLen)
}

func readClusterFrame(r io.Reader, hasPeerID bool, maxPayloadLen uint32) (*ClusterFrame, error) {
	var payloadLen uint32
	if err := binary.Read(r, binary.BigEndian, &payloadLen); err != nil {
		return nil, fmt.Errorf("reading cluster frame length: %w", err)
	}
	if err := validateClusterPayloadLen(uint64(payloadLen), maxPayloadLen); err != nil {
		return nil, err
	}

	frame := new(ClusterFrame)
	if hasPeerID {
		if err := binary.Read(r, binary.BigEndian, &frame.ClusterPeerID); err != nil {
			return nil, fmt.Errorf("reading cluster peer ID: %w", normalizeTruncatedEOF(err))
		}
	}

	body := make([]byte, frameBodyHeader+int(payloadLen))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("reading cluster frame body (payload_len=%d): %w", payloadLen, normalizeTruncatedEOF(err))
	}
	frame.Opcode = body[0]
	frame.RPCID = binary.BigEndian.Uint64(body[1:frameBodyHeader])
	frame.Payload = body[frameBodyHeader:]
	return frame, nil
}

func writeClusterFrame(w io.Writer, value Frame, withPeerID bool, maxPayloadLen uint32) error {
	frame, ok := value.(*ClusterFrame)
	if !ok || frame == nil {
		return fmt.Errorf("%w: expected cluster frame, got %T", ErrInvalidFrame, value)
	}
	if !withPeerID && frame.ClusterPeerID != 0 {
		return fmt.Errorf("%w: cluster peer ID is not supported by this frame codec", ErrInvalidFrame)
	}
	if err := validateClusterPayloadLen(uint64(len(frame.Payload)), maxPayloadLen); err != nil {
		return err
	}

	extra := 0
	if withPeerID {
		extra = clusterPeerIDSize
	}
	buf := make([]byte, frameHeader+extra+frameBodyHeader+len(frame.Payload))
	binary.BigEndian.PutUint32(buf[:frameHeader], uint32(len(frame.Payload)))
	offset := frameHeader
	if withPeerID {
		binary.BigEndian.PutUint64(buf[offset:offset+clusterPeerIDSize], frame.ClusterPeerID)
		offset += clusterPeerIDSize
	}
	buf[offset] = frame.Opcode
	binary.BigEndian.PutUint64(buf[offset+opcodeSize:offset+frameBodyHeader], frame.RPCID)
	copy(buf[offset+frameBodyHeader:], frame.Payload)
	_, err := w.Write(buf)
	return err
}

func validateClusterPayloadLen(payloadLen uint64, maxPayloadLen uint32) error {
	if maxPayloadLen > MaxP2PPayloadLen {
		return fmt.Errorf("%w: cluster frame maxPayloadLen %d exceeds P2P payload limit %d", ErrInvalidFrame, maxPayloadLen, MaxP2PPayloadLen)
	}
	if maxPayloadLen != 0 && payloadLen > uint64(maxPayloadLen) {
		return fmt.Errorf("%w: cluster frame payload too large: %d > %d", ErrInvalidFrame, payloadLen, maxPayloadLen)
	}
	if payloadLen > maxClusterPayloadLen {
		return fmt.Errorf("%w: cluster frame payload length %d exceeds capacity %d", ErrInvalidFrame, payloadLen, maxClusterPayloadLen)
	}
	return nil
}

// DecodeP2PFrame decodes one complete devp2p message payload. The caller must
// pass exactly one message's bytes. The fixed P2P size limit is always enforced.
// The returned Payload aliases data. Copy the payload if it must survive
// changes to or reuse of the input buffer.
func DecodeP2PFrame(data []byte) (*P2PFrame, error) {
	if len(data) > MaxP2PFrameSize {
		return nil, fmt.Errorf("P2P frame too large: %d > %d", len(data), MaxP2PFrameSize)
	}
	if len(data) < chainIDSize+frameBodyHeader {
		return nil, fmt.Errorf("P2P frame too short: %d < %d", len(data), chainIDSize+frameBodyHeader)
	}

	body := data[chainIDSize:]
	return &P2PFrame{
		ChainID: new(big.Int).SetBytes(data[:chainIDSize]),
		Opcode:  body[0],
		RPCID:   binary.BigEndian.Uint64(body[1:frameBodyHeader]),
		Payload: body[frameBodyHeader:],
	}, nil
}

// EncodeP2PFrame encodes one external cluster↔cluster message payload. The
// fixed P2P size limit is always enforced. This codec adds no length prefix.
func EncodeP2PFrame(value Frame) ([]byte, error) {
	frame, ok := value.(*P2PFrame)
	if !ok || frame == nil {
		return nil, fmt.Errorf("%w: expected P2P frame, got %T", ErrInvalidFrame, value)
	}
	if frame.ChainID == nil || frame.ChainID.Sign() < 0 || frame.ChainID.BitLen() > 256 {
		return nil, fmt.Errorf("%w: invalid routing chain ID: %v", ErrInvalidFrame, frame.ChainID)
	}
	if len(frame.Payload) > MaxP2PPayloadLen {
		return nil, fmt.Errorf("%w: P2P frame payload too large: %d > %d", ErrInvalidFrame, len(frame.Payload), MaxP2PPayloadLen)
	}
	buf := make([]byte, chainIDSize+frameBodyHeader+len(frame.Payload))
	frame.ChainID.FillBytes(buf[:chainIDSize])
	buf[chainIDSize] = frame.Opcode
	binary.BigEndian.PutUint64(buf[chainIDSize+opcodeSize:chainIDSize+frameBodyHeader], frame.RPCID)
	copy(buf[chainIDSize+frameBodyHeader:], frame.Payload)
	return buf, nil
}

// WriteP2PFrame writes one P2P frame to w.
func WriteP2PFrame(w io.Writer, frame Frame) error {
	buf, err := EncodeP2PFrame(frame)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// normalizeTruncatedEOF converts io.EOF to io.ErrUnexpectedEOF. Use it only for
// reads after the frame length has been consumed, where EOF means the frame was
// truncated. The length read must not use it, so that EOF before a frame starts
// stays io.EOF and signals a graceful disconnect.
func normalizeTruncatedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
