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
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/golang/snappy"
)

const testMaxPayloadLen = 1024

func TestClusterFrameRoundTrip(t *testing.T) {
	frame := &ClusterFrame{Opcode: 0x42, RPCID: 0x0102030405060708, Payload: []byte("chain")}
	var buf bytes.Buffer
	if err := WriteClusterFrame(&buf, frame); err != nil {
		t.Fatal(err)
	}
	encoded := buf.Bytes()
	if got := binary.BigEndian.Uint32(encoded[:4]); got != uint32(len(frame.Payload)) {
		t.Fatalf("payload length = %d, want %d", got, len(frame.Payload))
	}
	got, err := ReadClusterFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Opcode != frame.Opcode || got.RPCID != frame.RPCID || !bytes.Equal(got.Payload, frame.Payload) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, frame)
	}
}

func TestClusterFrameWithPeerIDRoundTrip(t *testing.T) {
	frame := &ClusterFrame{
		ClusterPeerID: 0x1122334455667788,
		Opcode:        0x81,
		RPCID:         9,
		Payload:       []byte("master-chain"),
	}
	var buf bytes.Buffer
	if err := WriteClusterFrameWithPeerID(&buf, frame, 0); err != nil {
		t.Fatal(err)
	}
	encoded := buf.Bytes()
	if got := binary.BigEndian.Uint64(encoded[4:12]); got != frame.ClusterPeerID {
		t.Fatalf("cluster peer ID = 0x%x, want 0x%x", got, frame.ClusterPeerID)
	}
	got, err := ReadClusterFrameWithPeerID(&buf, testMaxPayloadLen)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterPeerID != frame.ClusterPeerID || got.Opcode != frame.Opcode || got.RPCID != frame.RPCID || !bytes.Equal(got.Payload, frame.Payload) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, frame)
	}
}

func TestP2PFrameRoundTrip(t *testing.T) {
	frame := &P2PFrame{ChainID: big.NewInt(0x01020304), Opcode: 0x02, RPCID: 7, Payload: []byte("p2p")}
	encoded, err := EncodeP2PFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != chainIDSize+frameBodyHeader+len(frame.Payload) {
		t.Fatalf("encoded length = %d", len(encoded))
	}
	got, err := DecodeP2PFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChainID.Cmp(frame.ChainID) != 0 || got.Opcode != frame.Opcode || got.RPCID != frame.RPCID || !bytes.Equal(got.Payload, frame.Payload) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, frame)
	}
}

func TestP2PFrameLargeChainID(t *testing.T) {
	id := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	encoded, err := EncodeP2PFrame(&P2PFrame{ChainID: id})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := DecodeP2PFrame(encoded)
	if err != nil || frame.ChainID.Cmp(id) != 0 {
		t.Fatalf("chain ID truncated: frame %v, error %v", frame, err)
	}
	clear(encoded[:chainIDSize])
	if frame.ChainID.Cmp(id) != 0 {
		t.Fatal("decoded chain ID aliases the input")
	}
	for _, invalid := range []*big.Int{nil, big.NewInt(-1), new(big.Int).Lsh(big.NewInt(1), 256)} {
		if _, err := EncodeP2PFrame(&P2PFrame{ChainID: invalid}); err == nil {
			t.Fatalf("accepted invalid chain ID %v", invalid)
		}
	}
}

func TestP2PFramePayloadAliasesInput(t *testing.T) {
	var encoded bytes.Buffer
	if err := WriteP2PFrame(&encoded, &P2PFrame{ChainID: big.NewInt(10001), Payload: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	frame, err := DecodeP2PFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] = 'z'
	if !bytes.Equal(frame.Payload, []byte("abz")) {
		t.Fatalf("payload = %q, want input buffer changes to be visible", frame.Payload)
	}
}

func TestRootChainID(t *testing.T) {
	first := &P2PFrame{ChainID: RootChainID()}
	second := &P2PFrame{ChainID: RootChainID()}
	first.ChainID.SetUint64(10001)
	if second.ChainID.Sign() != 0 || RootChainID().Sign() != 0 {
		t.Fatal("changing a frame's chain ID changed another root chain ID")
	}
	var encoded bytes.Buffer
	if err := WriteP2PFrame(&encoded, second); err != nil {
		t.Fatal(err)
	}
	frame, err := DecodeP2PFrame(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if frame.ChainID.Sign() != 0 {
		t.Fatalf("decoded chain ID = %d, want zero", frame.ChainID)
	}
}

func TestFrameWireLayouts(t *testing.T) {
	const peerID = 0x1122334455667788
	frame := &ClusterFrame{Opcode: 0x10, RPCID: 0x0102030405060708, Payload: []byte{0xaa}}

	var chain bytes.Buffer
	if err := WriteClusterFrame(&chain, frame); err != nil {
		t.Fatal(err)
	}
	wantChain := []byte{0, 0, 0, 1, 0x10, 1, 2, 3, 4, 5, 6, 7, 8, 0xaa}
	if !bytes.Equal(chain.Bytes(), wantChain) {
		t.Fatalf("ClusterFrame bytes = %x, want %x", chain.Bytes(), wantChain)
	}

	frame.ClusterPeerID = peerID
	var master bytes.Buffer
	if err := WriteClusterFrameWithPeerID(&master, frame, 0); err != nil {
		t.Fatal(err)
	}
	wantMaster := []byte{0, 0, 0, 1, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x10, 1, 2, 3, 4, 5, 6, 7, 8, 0xaa}
	if !bytes.Equal(master.Bytes(), wantMaster) {
		t.Fatalf("ClusterFrame with peer ID bytes = %x, want %x", master.Bytes(), wantMaster)
	}

	var p2p bytes.Buffer
	if err := WriteP2PFrame(&p2p, &P2PFrame{ChainID: big.NewInt(0xaabbccdd), Opcode: frame.Opcode, RPCID: frame.RPCID, Payload: frame.Payload}); err != nil {
		t.Fatal(err)
	}
	wantP2P := append(make([]byte, 28), []byte{0xaa, 0xbb, 0xcc, 0xdd, 0x10, 1, 2, 3, 4, 5, 6, 7, 8, 0xaa}...)
	if !bytes.Equal(p2p.Bytes(), wantP2P) {
		t.Fatalf("P2PFrame bytes = %x, want %x", p2p.Bytes(), wantP2P)
	}
}

func TestMultiFrameClusterStream(t *testing.T) {
	frames := []*ClusterFrame{
		{Opcode: 1, RPCID: 1, Payload: []byte("one")},
		{Opcode: 2, RPCID: 2, Payload: []byte("two")},
	}
	var stream bytes.Buffer
	for _, frame := range frames {
		if err := WriteClusterFrame(&stream, frame); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range frames {
		got, err := ReadClusterFrame(&stream)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Opcode != want.Opcode || got.RPCID != want.RPCID || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestFrameReadErrors(t *testing.T) {
	if _, err := ReadClusterFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream error = %v, want EOF", err)
	}
	if _, err := ReadClusterFrameWithPeerID(bytes.NewReader([]byte{0, 0}), testMaxPayloadLen); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated length error = %v, want unexpected EOF", err)
	}
	var buf bytes.Buffer
	if err := WriteClusterFrameWithPeerID(&buf, &ClusterFrame{Payload: []byte("payload")}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClusterFrameWithPeerID(bytes.NewReader(buf.Bytes()), 3); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, err := DecodeP2PFrame([]byte{1, 2, 3}); err == nil {
		t.Fatal("short P2P frame accepted")
	}
	if err := WriteClusterFrame(&buf, &ClusterFrame{ClusterPeerID: 1}); err == nil {
		t.Fatal("direct chain codec accepted a cluster peer ID")
	}
	if _, err := EncodeP2PFrame(nil); err == nil {
		t.Fatal("EncodeP2PFrame accepted a nil frame")
	}
}

func TestFrameCodecRejectsInvalidTypes(t *testing.T) {
	for _, codec := range []struct {
		name     string
		write    func(io.Writer, Frame) error
		wrong    Frame
		typedNil Frame
	}{
		{"cluster", WriteClusterFrame, &P2PFrame{ChainID: RootChainID()}, (*ClusterFrame)(nil)},
		{"cluster with peer ID", func(w io.Writer, frame Frame) error { return WriteClusterFrameWithPeerID(w, frame, 0) }, &P2PFrame{ChainID: RootChainID()}, (*ClusterFrame)(nil)},
		{"P2P", WriteP2PFrame, &ClusterFrame{}, (*P2PFrame)(nil)},
	} {
		t.Run(codec.name, func(t *testing.T) {
			for _, frame := range []Frame{nil, codec.typedNil, codec.wrong} {
				var encoded bytes.Buffer
				if err := codec.write(&encoded, frame); !errors.Is(err, ErrInvalidFrame) {
					t.Fatalf("frame %T: error = %v, want invalid frame", frame, err)
				}
				if encoded.Len() != 0 {
					t.Fatalf("frame %T: wrote %d bytes before rejection", frame, encoded.Len())
				}
			}
		})
	}
}

func TestFrameEOFContract(t *testing.T) {
	if _, err := ReadClusterFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("clean EOF = %v, want EOF", err)
	}
	if _, err := ReadClusterFrame(bytes.NewReader([]byte{0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial length = %v, want unexpected EOF", err)
	}
	if _, err := ReadClusterFrameWithPeerID(bytes.NewReader([]byte{0, 0, 0, 0}), testMaxPayloadLen); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("missing cluster peer ID = %v, want unexpected EOF", err)
	}
	if _, err := ReadClusterFrame(bytes.NewReader([]byte{0, 0, 0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("missing frame body = %v, want unexpected EOF", err)
	}
	var truncated bytes.Buffer
	if err := WriteClusterFrame(&truncated, &ClusterFrame{Payload: []byte("payload")}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClusterFrame(bytes.NewReader(truncated.Bytes()[:truncated.Len()-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload = %v, want unexpected EOF", err)
	}
}

func TestClusterFrameFinalReadError(t *testing.T) {
	readErr := errors.New("read failed")
	codecs := []struct {
		name  string
		read  func(io.Reader) (*ClusterFrame, error)
		write func(io.Writer, Frame) error
	}{
		{"direct", ReadClusterFrame, WriteClusterFrame},
		{"peer", func(r io.Reader) (*ClusterFrame, error) { return ReadClusterFrameWithPeerID(r, 0) }, func(w io.Writer, frame Frame) error { return WriteClusterFrameWithPeerID(w, frame, 0) }},
	}
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			frame := &ClusterFrame{Opcode: 0x42, RPCID: 7, Payload: []byte("payload")}
			var encoded bytes.Buffer
			if err := codec.write(&encoded, frame); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name    string
				cut     int
				readErr error
				wantErr error
			}{
				{"complete with error", encoded.Len(), readErr, nil},
				{"complete with EOF", encoded.Len(), io.EOF, nil},
				{"truncated with error", encoded.Len() - 1, readErr, readErr},
				{"truncated with EOF", encoded.Len() - 1, io.EOF, io.ErrUnexpectedEOF},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := &finalErrorReader{data: encoded.Bytes()[:tc.cut], err: tc.readErr}
					got, err := codec.read(r)
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("read error = %v, want %v", err, tc.wantErr)
					}
					if tc.wantErr == nil && (got == nil || got.Opcode != frame.Opcode || got.RPCID != frame.RPCID || !bytes.Equal(got.Payload, frame.Payload)) {
						t.Fatalf("complete frame = %+v, want %+v", got, frame)
					}
				})
			}
		})
	}
}

type finalErrorReader struct {
	data []byte
	err  error
}

func (r *finalErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestFramePayloadLimits(t *testing.T) {
	for _, codec := range []struct {
		name  string
		read  func(io.Reader, uint32) (*ClusterFrame, error)
		write func(io.Writer, Frame, uint32) error
	}{
		{"peer", ReadClusterFrameWithPeerID, WriteClusterFrameWithPeerID},
	} {
		t.Run(codec.name, func(t *testing.T) {
			if _, err := codec.read(bytes.NewReader(nil), 0); !errors.Is(err, io.EOF) {
				t.Fatalf("unlimited empty stream error = %v, want EOF", err)
			}
			frame := &ClusterFrame{Payload: []byte("payload")}
			var encoded bytes.Buffer
			if err := codec.write(&encoded, frame, uint32(len(frame.Payload))); err != nil {
				t.Fatal(err)
			}
			if _, err := codec.read(bytes.NewReader(encoded.Bytes()), 3); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("read above configured limit: %v", err)
			}
			encoded.Reset()
			if err := codec.write(&encoded, frame, 3); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("write above configured limit: %v", err)
			}
			if encoded.Len() != 0 {
				t.Fatal("writer emitted bytes before rejecting oversized payload")
			}
		})
	}
}

func TestClusterFrameConfiguredLimit(t *testing.T) {
	for _, limit := range []uint32{0, MaxP2PPayloadLen, MaxP2PPayloadLen + 1, math.MaxUint32} {
		var encoded bytes.Buffer
		if err := WriteClusterFrameWithPeerID(&encoded, &ClusterFrame{}, 0); err != nil {
			t.Fatal(err)
		}
		_, readErr := ReadClusterFrameWithPeerID(bytes.NewReader(encoded.Bytes()), limit)
		var output bytes.Buffer
		writeErr := WriteClusterFrameWithPeerID(&output, &ClusterFrame{}, limit)
		if limit <= MaxP2PPayloadLen {
			if readErr != nil || writeErr != nil {
				t.Fatalf("valid limit %d: read = %v, write = %v", limit, readErr, writeErr)
			}
		} else {
			if !errors.Is(readErr, ErrInvalidFrame) || !errors.Is(writeErr, ErrInvalidFrame) {
				t.Fatalf("invalid limit %d: read = %v, write = %v", limit, readErr, writeErr)
			}
			if output.Len() != 0 {
				t.Fatalf("invalid limit %d: wrote %d bytes before rejection", limit, output.Len())
			}
		}
	}
}

func TestClusterFrameUnlimitedPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5}, (16<<20)+1)
	for _, codec := range []struct {
		name   string
		peerID uint64
		read   func(io.Reader) (*ClusterFrame, error)
		write  func(io.Writer, Frame) error
	}{
		{"direct", 0, ReadClusterFrame, WriteClusterFrame},
		{"peer", math.MaxUint64, func(r io.Reader) (*ClusterFrame, error) { return ReadClusterFrameWithPeerID(r, 0) }, func(w io.Writer, frame Frame) error { return WriteClusterFrameWithPeerID(w, frame, 0) }},
	} {
		t.Run(codec.name, func(t *testing.T) {
			frame := &ClusterFrame{ClusterPeerID: codec.peerID, Opcode: 0x42, RPCID: 7, Payload: payload}
			var encoded bytes.Buffer
			if err := codec.write(&encoded, frame); err != nil {
				t.Fatal(err)
			}
			got, err := codec.read(&encoded)
			if err != nil {
				t.Fatal(err)
			}
			if got.ClusterPeerID != frame.ClusterPeerID || got.Opcode != frame.Opcode || got.RPCID != frame.RPCID || !bytes.Equal(got.Payload, payload) {
				t.Fatal("large internal frame round trip mismatch")
			}
		})
	}
}

func TestClusterFrameLengthCapacity(t *testing.T) {
	if err := validateClusterPayloadLen(maxClusterPayloadLen, 0); err != nil {
		t.Fatalf("length at capacity: %v", err)
	}
	if err := validateClusterPayloadLen(maxClusterPayloadLen+1, 0); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("length exceeding capacity: %v", err)
	}
	if err := validateClusterPayloadLen(uint64(math.MaxUint32)+1, 0); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("length exceeding uint32: %v", err)
	}
}

func TestP2PFramePayloadLimit(t *testing.T) {
	codeSize := rlp.IntSize(math.MaxUint64)
	if codeSize != rlpxMaxCodeSize || MaxP2PFrameSize+codeSize > rlpxMaxFrameSize ||
		snappy.MaxEncodedLen(MaxP2PFrameSize)+codeSize > rlpxMaxFrameSize {
		t.Fatal("P2P limit does not fit RLPx with and without Snappy")
	}
	payload := make([]byte, MaxP2PPayloadLen)
	if _, err := rand.New(rand.NewSource(1)).Read(payload); err != nil {
		t.Fatal(err)
	}
	chainID := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	external := &P2PFrame{ChainID: chainID, Opcode: math.MaxUint8, RPCID: math.MaxUint64, Payload: payload}
	encoded, err := EncodeP2PFrame(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != MaxP2PFrameSize {
		t.Fatalf("maximum frame size = %d, want %d", len(encoded), MaxP2PFrameSize)
	}
	compressed := snappy.Encode(nil, encoded)
	if len(compressed)+codeSize > rlpxMaxFrameSize {
		t.Fatalf("compressed complete frame plus message code = %d, exceeds RLPx limit %d", len(compressed)+codeSize, rlpxMaxFrameSize)
	}
	decompressed, err := snappy.Decode(nil, compressed)
	if err != nil || !bytes.Equal(decompressed, encoded) {
		t.Fatalf("compressed frame round trip: %v", err)
	}
	received, err := DecodeP2PFrame(encoded)
	if err != nil {
		t.Fatal(err)
	}
	internal := &ClusterFrame{ClusterPeerID: math.MaxUint64, Opcode: received.Opcode, RPCID: received.RPCID, Payload: received.Payload}
	var stream bytes.Buffer
	if err := WriteClusterFrameWithPeerID(&stream, internal, MaxP2PPayloadLen); err != nil {
		t.Fatal(err)
	}
	relayed, err := ReadClusterFrameWithPeerID(&stream, MaxP2PPayloadLen)
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := EncodeP2PFrame(&P2PFrame{
		ChainID: external.ChainID, Opcode: relayed.Opcode, RPCID: relayed.RPCID, Payload: relayed.Payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if relayed.ClusterPeerID != math.MaxUint64 || !bytes.Equal(encoded, outbound) {
		t.Fatal("maximum peer frame changed during cluster round trip")
	}
	external.Payload = append(payload, 0)
	if _, err := EncodeP2PFrame(external); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversized P2P encode: %v", err)
	}
	var rejected bytes.Buffer
	if err := WriteP2PFrame(&rejected, external); !errors.Is(err, ErrInvalidFrame) || rejected.Len() != 0 {
		t.Fatalf("oversized P2P write: error = %v, bytes = %d", err, rejected.Len())
	}
	if _, err := DecodeP2PFrame(append(encoded, 0)); err == nil {
		t.Fatal("decoder accepted an oversized P2P frame")
	}
	if err := WriteClusterFrameWithPeerID(&rejected, &ClusterFrame{Payload: external.Payload}, MaxP2PPayloadLen); !errors.Is(err, ErrInvalidFrame) || rejected.Len() != 0 {
		t.Fatalf("oversized virtual-peer write: error = %v, bytes = %d", err, rejected.Len())
	}
}

func TestClusterFrameForgedLength(t *testing.T) {
	for _, codec := range []struct {
		name string
		read func(io.Reader, uint32) (*ClusterFrame, error)
	}{
		{"peer", ReadClusterFrameWithPeerID},
	} {
		t.Run(codec.name, func(t *testing.T) {
			for _, length := range []uint32{MaxP2PPayloadLen + 1, math.MaxUint32} {
				// Leave bytes after the header to detect reads of peer ID or body.
				var data [frameHeader + clusterPeerIDSize + frameBodyHeader]byte
				binary.BigEndian.PutUint32(data[:frameHeader], length)
				r := bytes.NewReader(data[:])
				if _, err := codec.read(r, MaxP2PPayloadLen); err == nil || !strings.Contains(err.Error(), "payload too large") {
					t.Fatalf("forged length %d error = %v, want payload too large", length, err)
				}
				if r.Len() != len(data)-frameHeader {
					t.Fatalf("read beyond length header for forged length %d", length)
				}
			}
		})
	}
}
