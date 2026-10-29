// Copyright 2026-2027, QuarkChain.

package conn

import (
	"fmt"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/wire"
)

// Config holds immutable connection configuration.
type Config struct {
	// Transport is the frame I/O backend. Required.
	Transport FrameTransport

	// Serializers maps locally supported request opcodes to their serializers.
	// It may include outbound-only requests which have no entry in Handlers.
	// RPC response opcodes are kept in a separate internal table;
	// non-RPC serializers (see NonRPCOps) have no response opcode.
	// A response must match both the pending RPC ID and its ResponseOpCode.
	// All serializer callbacks are required.
	Serializers map[byte]*OpSerializer

	// Handlers maps inbound request opcodes to their handlers. Its keys define
	// which request opcodes this endpoint accepts. Requests without a handler
	// are rejected, even if their serializer is registered for outbound use.
	// Must be safe for concurrent use: handlers may be called from multiple
	// dispatch goroutines, in a different order from frame arrival. Business
	// operations that require ordering must serialize their own execution.
	// The external peer ingress layer must bound admitted work; dispatch itself
	// does not impose a concurrency limit.
	Handlers map[byte]TypedHandler

	// NonRPCOps marks fire-and-forget opcodes that must use rpc_id=0.
	// Each opcode must have a request serializer in Serializers.
	NonRPCOps map[byte]struct{}

	// Forwarder optionally routes inbound frames before normal dispatch.
	//
	// It runs synchronously on the reader goroutine. Handoff must be nonblocking;
	// a full destination queue requires an explicit ingress policy. Blocking the
	// callback prevents all RPC responses and virtual peers on this link from
	// making progress.
	//
	// On a multiplexed physical connection, it must consume every frame with a
	// nonzero ClusterPeerID, including responses and unknown or destroyed routes.
	// Such frames must never fall through to local dispatch. Reject unknown routes
	// according to the routing layer's policy and return true.
	//
	// To close the connection, call c.Close() directly. It does not wait for
	// the reader loop, but it calls transport.Close synchronously. Return true
	// to prevent the current frame from being dispatched after closing.
	//
	// The callback is responsible for synchronizing any shared state it accesses.
	Forwarder func(*wire.ClusterFrame) bool

	// Logger defaults to log.Root() if nil.
	Logger log.Logger
}

// validate checks configuration invariants.
func (cfg *Config) validate() {
	if cfg.Transport == nil {
		panic("conn.Config: Transport must not be nil")
	}
	for op := range cfg.NonRPCOps {
		if _, ok := cfg.Serializers[op]; !ok {
			panic(fmt.Sprintf("conn.Config: serializer for non-RPC opcode 0x%x must be configured", op))
		}
	}

	// Every configured opcode is a request opcode, whether sent or handled by
	// this endpoint. Response opcodes must stay disjoint from them and unique:
	// otherwise request/response classification would be ambiguous or a
	// response serializer would depend on map-iteration order.
	requestOps := make(map[byte]struct{}, len(cfg.Serializers))
	for op, ser := range cfg.Serializers {
		if ser == nil {
			panic(fmt.Sprintf("conn.Config: serializer for opcode 0x%x must not be nil", op))
		}
		// Missing callbacks would panic at runtime on the reader goroutine.
		if ser.NewRequest == nil || ser.NewResponse == nil || ser.Deserialize == nil || ser.Serialize == nil {
			panic(fmt.Sprintf("conn.Config: serializer for opcode 0x%x has missing callback", op))
		}
		requestOps[op] = struct{}{}
	}

	responseOwners := make(map[byte]byte)
	for op, ser := range cfg.Serializers {
		// Non-RPC commands do not have responses. Their ResponseOpCode is ignored
		// and is not registered as a response serializer.
		if _, ok := cfg.NonRPCOps[op]; ok {
			continue
		}

		if ser.ResponseOpCode == 0 {
			panic(fmt.Sprintf("conn.Config: serializer ResponseOpCode for opcode 0x%x must not be zero", op))
		}
		if _, conflict := requestOps[ser.ResponseOpCode]; conflict {
			panic(fmt.Sprintf("conn.Config: response opcode 0x%x conflicts with a request opcode", ser.ResponseOpCode))
		}
		if owner, exists := responseOwners[ser.ResponseOpCode]; exists {
			panic(fmt.Sprintf("conn.Config: response opcode 0x%x is used by multiple request opcodes 0x%x and 0x%x", ser.ResponseOpCode, owner, op))
		}

		responseOwners[ser.ResponseOpCode] = op
	}

	for op, h := range cfg.Handlers {
		if h == nil {
			panic(fmt.Sprintf("conn.Config: handler for opcode 0x%x must not be nil", op))
		}
		if _, ok := cfg.Serializers[op]; !ok {
			panic(fmt.Sprintf("conn.Config: serializer for handler opcode 0x%x must be configured", op))
		}
	}
}
