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

package slaveconn

import (
	"context"
	"errors"
	"io"
	"maps"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/qkc/conn"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/qkc/wire"
	"github.com/holiman/uint256"
)

var (
	_ ShardBackend        = (*shard.Shard)(nil)
	_ shard.ClusterClient = (*Service)(nil)
)

type serviceBackend struct {
	inits    atomic.Int32
	initErr  error
	rootErr  error
	roots    atomic.Int32
	deposits chan common.Hash
	entered  chan struct{}
	release  chan struct{}
}

func (b *serviceBackend) Init(*qkctypes.RootBlock) error { b.inits.Add(1); return b.initErr }
func (b *serviceBackend) AddRootBlock(*qkctypes.RootBlock) (bool, error) {
	b.roots.Add(1)
	if b.entered != nil {
		close(b.entered)
		<-b.release
	}
	return false, b.rootErr
}
func (b *serviceBackend) GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error) {
	return uint256.NewInt(42), common.Address{1}, nil
}
func (b *serviceBackend) GetUnconfirmedHeaderList() ([]*types.Header, error) { return nil, nil }
func (b *serviceBackend) AddXshardTxList(hash common.Hash, _ []*types.DepositTx) error {
	if b.entered != nil {
		close(b.entered)
		<-b.release
	}
	if b.deposits != nil {
		b.deposits <- hash
	}
	return nil
}

func newTestService(t *testing.T, id int64, backend *serviceBackend, shutdown func()) *Service {
	t.Helper()
	// The owner schedules shutdown asynchronously; Service deduplicates failures.
	requestShutdown := func() { go shutdown() }
	s, err := NewService(ServiceConfig{ListenAddr: "127.0.0.1:0", LocalChainID: big.NewInt(id), ConfiguredChainIDs: []*big.Int{big.NewInt(1), big.NewInt(2)}, Shutdown: requestShutdown})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(backend); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func serviceMaster(t *testing.T, s *Service, handler conn.TypedHandler) *conn.BaseConn {
	t.Helper()
	nc, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if handler == nil {
		handler = func(wire.SSZMessage) (wire.SSZMessage, error) {
			return &wire.AddShardBlockHeaderResponse{ErrorCode: 9}, nil
		}
	}
	remote := conn.NewBaseConn(conn.Config{
		Transport: conn.NewTCPTransport(nc, func(r io.Reader) (*wire.ClusterFrame, error) {
			return wire.ReadClusterFrameWithPeerID(r, wire.MaxFramePayloadLen)
		}, wire.WriteClusterFrameWithPeerID),
		Serializers: map[byte]*conn.OpSerializer{
			byte(wire.ClusterOpPing):                         conn.OpSerializerFor[wire.PingRequest, wire.PongResponse](byte(wire.ClusterOpPong)),
			byte(wire.ClusterOpConnectToSlavesRequest):       conn.OpSerializerFor[wire.ConnectToSlavesRequest, wire.ConnectToSlavesResponse](byte(wire.ClusterOpConnectToSlavesResponse)),
			byte(wire.ClusterOpAddRootBlockRequest):          conn.OpSerializerFor[wire.AddRootBlockRequest, wire.AddRootBlockResponse](byte(wire.ClusterOpAddRootBlockResponse)),
			byte(wire.ClusterOpGetRootChainStakesRequest):    conn.OpSerializerFor[wire.GetRootChainStakesRequest, wire.GetRootChainStakesResponse](byte(wire.ClusterOpGetRootChainStakesResponse)),
			byte(wire.ClusterOpGetUnconfirmedHeadersRequest): conn.OpSerializerFor[wire.GetUnconfirmedHeadersRequest, wire.GetUnconfirmedHeadersResponse](byte(wire.ClusterOpGetUnconfirmedHeadersResponse)),
			byte(wire.ClusterOpAddShardBlockHeaderRequest):   conn.OpSerializerFor[wire.AddShardBlockHeaderRequest, wire.AddShardBlockHeaderResponse](byte(wire.ClusterOpAddShardBlockHeaderResponse)),
		},
		Handlers: map[byte]conn.TypedHandler{byte(wire.ClusterOpAddShardBlockHeaderRequest): handler},
	})
	remote.Start()
	t.Cleanup(remote.Close)
	// A round trip ensures that the Master slot is assigned before xshard dials.
	serviceRPC(t, remote, wire.ClusterOpPing, &wire.PingRequest{})
	return remote
}

func serviceRPC(t *testing.T, remote *conn.BaseConn, op wire.ClusterOp, req wire.SSZMessage) wire.SSZMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := remote.SendRPC(ctx, byte(op), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func initializeService(t *testing.T, remote *conn.BaseConn) []byte {
	t.Helper()
	root := qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{})
	data, err := root.MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	serviceRPC(t, remote, wire.ClusterOpPing, &wire.PingRequest{HasRootTip: true, RootTip: data})
	return data
}

func TestServiceClusterRoundTrip(t *testing.T) {
	aBackend := &serviceBackend{rootErr: &comm.RemoteError{Code: 9}}
	bBackend := &serviceBackend{deposits: make(chan common.Hash, 1)}
	shutdown := make(chan struct{})
	a, b := newTestService(t, 1, aBackend, func() { close(shutdown) }), newTestService(t, 2, bBackend, noopShutdown)
	am, bm := serviceMaster(t, a, nil), serviceMaster(t, b, nil)
	if aBackend.inits.Load() != 0 || bBackend.inits.Load() != 0 {
		t.Fatal("initial PING must not initialize shards")
	}

	// Ask the service to connect through the real Master topology handler.
	addr := b.Addr().(*net.TCPAddr)
	chainID, _ := wire.ChainIDToSSZ(big.NewInt(2))
	response := serviceRPC(t, am, wire.ClusterOpConnectToSlavesRequest, &wire.ConnectToSlavesRequest{
		SlaveInfoList: []wire.SlaveInfo{{Host: []byte("127.0.0.1"), Port: uint16(addr.Port), ChainID: chainID}},
	}).(*wire.ConnectToSlavesResponse)
	if len(response.ResultList) != 1 || len(response.ResultList[0]) != 0 {
		t.Fatalf("dial response: %+v", response)
	}
	if aBackend.inits.Load() != 0 || bBackend.inits.Load() != 0 {
		t.Fatal("interconnection must not initialize shards")
	}
	// Only the subsequent PING(rootTip) initializes each already constructed shard.
	root := initializeService(t, am)
	initializeService(t, bm)
	if aBackend.inits.Load() != 1 || bBackend.inits.Load() != 1 {
		t.Fatal("root PING did not initialize shards")
	}
	hash := common.Hash{3}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.BroadcastXShardTxList(ctx, big.NewInt(2), hash, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-bBackend.deposits:
		if got != hash {
			t.Fatalf("deposit block hash: %v", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	err := a.SendShardBlockHeaderToMaster(ctx, &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}, uint256.NewInt(7))
	var remoteErr *comm.RemoteError
	if !errors.As(err, &remoteErr) || remoteErr.Code != 9 {
		t.Fatalf("master rejection lost: %v", err)
	}
	if err := a.BroadcastXShardTxList(ctx, big.NewInt(3), hash, nil); err == nil {
		t.Fatal("missing connection accepted")
	}

	result := serviceRPC(t, am, wire.ClusterOpAddRootBlockRequest, &wire.AddRootBlockRequest{RootBlock: root}).(*wire.AddRootBlockResponse)
	if result.ErrorCode != 9 || aBackend.roots.Load() != 1 {
		t.Fatal("backend failure was not returned")
	}
	// Business rejection must leave the connection usable.
	stakes := serviceRPC(t, am, wire.ClusterOpGetRootChainStakesRequest, &wire.GetRootChainStakesRequest{}).(*wire.GetRootChainStakesResponse)
	if stakes.ErrorCode != 0 || stakes.Stakes[0] != 42 || stakes.Signer != (common.Address{1}) {
		t.Fatalf("stakes adapter: %+v", stakes)
	}
	headers := serviceRPC(t, am, wire.ClusterOpGetUnconfirmedHeadersRequest, &wire.GetUnconfirmedHeadersRequest{}).(*wire.GetUnconfirmedHeadersResponse)
	if headers.ErrorCode != 0 || len(headers.HeaderList) != 0 {
		t.Fatalf("headers adapter: %+v", headers)
	}

	am.Close()
	select {
	case <-shutdown:
	case <-ctx.Done():
		t.Fatal("master loss did not notify owner")
	}
}

func TestServiceBackendErrorClosesConnection(t *testing.T) {
	backend := &serviceBackend{rootErr: errors.New("unknown root parent")}
	s := newTestService(t, 1, backend, noopShutdown)
	remote := serviceMaster(t, s, nil)
	root := initializeService(t, remote)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := remote.SendRPC(ctx, byte(wire.ClusterOpAddRootBlockRequest), &wire.AddRootBlockRequest{RootBlock: root}); err == nil {
		t.Fatal("backend failure was not returned")
	}
	select {
	case <-remote.WaitUntilClosed():
	case <-ctx.Done():
		t.Fatal("backend failure did not close connection")
	}
}

func TestServiceStopWaitsForBackend(t *testing.T) {
	backend := &serviceBackend{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(backend.release) }) }
	defer release()
	s := newTestService(t, 1, backend, noopShutdown)
	remote := serviceMaster(t, s, nil)
	root := initializeService(t, remote)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := remote.SendRPC(ctx, byte(wire.ClusterOpAddRootBlockRequest), &wire.AddRootBlockRequest{RootBlock: root})
		done <- err
	}()
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backend not called")
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	// Listener closure proves Stop has disabled admission before waiting.
	listenerClosed := make(chan error, 1)
	go func() {
		_, err := s.listener.Accept()
		listenerClosed <- err
	}()
	select {
	case err := <-listenerClosed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("listener close error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not close listener")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while backend was running")
	default:
	}
	select {
	case err := <-done:
		if !errors.Is(err, conn.ErrConnectionClosed) {
			t.Fatalf("pending request after stopping: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Stop did not close master connection")
	}
	if _, err := remote.SendRPC(ctx, byte(wire.ClusterOpAddRootBlockRequest), &wire.AddRootBlockRequest{RootBlock: root}); !errors.Is(err, conn.ErrConnectionClosed) {
		t.Fatalf("new request after stopping: %v", err)
	}
	release()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after backend finished")
	}

}

// Even a connection rejected before PING may have dispatched a business call.
func TestServiceStopWaitsForRejectedXshardHandlers(t *testing.T) {
	backend := &serviceBackend{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(backend.release) }) }
	defer release()
	s := newTestService(t, 1, backend, noopShutdown)
	serviceMaster(t, s, nil)
	nc, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := newXshardConn(nc, 0, big.NewInt(2), big.NewInt(1), true, &serviceBackend{}, nil)
	if err != nil {
		nc.Close()
		t.Fatal(err)
	}
	peer.Start()
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- peer.SendAddXshardTxList(ctx, common.Hash{}, nil) }()
	select {
	case <-backend.entered:
	case <-ctx.Done():
		t.Fatal("xshard handler not entered")
	}
	// No PING was sent. Closing causes handshake rejection and index removal.
	peer.Close()
	for {
		s.xshardManager.mu.RLock()
		remaining := len(s.xshardManager.connections)
		s.xshardManager.mu.RUnlock()
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("closed handshaking connection was not removed")
		case <-time.After(time.Millisecond):
		}
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop missed handler on rejected connection")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("Stop did not finish after xshard handler returned")
	}
	select {
	case err := <-result:
		if !errors.Is(err, conn.ErrConnectionClosed) {
			t.Fatalf("closed request: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pending RPC was not closed")
	}
}

func TestServiceStopDoesNotRequestShutdown(t *testing.T) {
	var calls atomic.Int32
	s, err := NewService(ServiceConfig{ListenAddr: "127.0.0.1:0", LocalChainID: big.NewInt(1), ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: func() { calls.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(&serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	serviceMaster(t, s, nil)
	s.Stop()
	// Exercise late notifications too, irrespective of watcher scheduling.
	s.shutdown()
	s.shutdown()
	if calls.Load() != 0 {
		t.Fatalf("normal Stop requested shutdown %d times", calls.Load())
	}
}

func TestServiceStopClosesPendingRPC(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	shutdown := make(chan struct{})
	s := newTestService(t, 1, &serviceBackend{}, func() { close(shutdown) })
	remote := serviceMaster(t, s, func(wire.SSZMessage) (wire.SSZMessage, error) {
		close(entered)
		<-release
		return &wire.AddShardBlockHeaderResponse{}, nil
	})
	initializeService(t, remote)
	result := make(chan error, 1)
	go func() {
		result <- s.SendShardBlockHeaderToMaster(context.Background(), &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}, nil)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("master did not receive report")
	}
	s.Stop()
	select {
	case err := <-result:
		if !errors.Is(err, conn.ErrConnectionClosed) {
			t.Fatalf("pending RPC error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending RPC not canceled")
	}
}

func TestServiceAssembly(t *testing.T) {
	cfg := ServiceConfig{ListenAddr: "127.0.0.1:0", LocalChainID: big.NewInt(1), ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown}
	s, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if s.Addr() != nil {
		t.Fatal("constructor opened listener")
	}
	if err := s.Start(nil); err == nil {
		t.Fatal("started with nil backend")
	}
	cfg.LocalChainID.SetInt64(99)
	if s.cfg.LocalChainID.Int64() != 1 {
		t.Fatal("configuration ID was not copied")
	}
	if err := s.Start(&serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(&serviceBackend{}); err == nil {
		t.Fatal("started twice")
	}
	s.Stop()
	if err := s.Start(&serviceBackend{}); err == nil {
		t.Fatal("restarted stopped service")
	}
}

func TestServiceStartListenFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s, err := NewService(ServiceConfig{ListenAddr: listener.Addr().String(), LocalChainID: big.NewInt(1), ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Start(&serviceBackend{}); err == nil {
		t.Fatal("started on occupied address")
	}
	if s.backend != nil || s.Addr() != nil {
		t.Fatal("failed start retained backend or listener")
	}
	listener.Close()
	backend := &serviceBackend{}
	if err := s.Start(backend); err != nil {
		t.Fatal(err)
	}
	if s.backend != backend {
		t.Fatal("retry did not install backend")
	}
}

func TestServiceInitializationErrorClosesConnection(t *testing.T) {
	for _, initErr := range []error{errors.New("checkpoint unavailable"), &comm.RemoteError{Code: 9}} {
		t.Run(initErr.Error(), func(t *testing.T) {
			backend := &serviceBackend{initErr: initErr}
			shutdown := make(chan struct{}, 1)
			s := newTestService(t, 1, backend, func() {
				select {
				case shutdown <- struct{}{}:
				default:
				}
			})
			remote := serviceMaster(t, s, nil)
			root := qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{})
			data, err := root.MarshalSSZ()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := remote.SendRPC(ctx, byte(wire.ClusterOpPing), &wire.PingRequest{HasRootTip: true, RootTip: data}); err == nil {
				t.Fatal("initialization failure accepted")
			}
			select {
			case <-remote.WaitUntilClosed():
			case <-ctx.Done():
				t.Fatal("initialization failure did not close connection")
			}
			select {
			case <-shutdown:
			case <-ctx.Done():
				t.Fatal("initialization failure did not request shutdown")
			}
			if backend.inits.Load() != 1 {
				t.Fatal("initialization was not forwarded once")
			}
		})
	}
}

// Fault notification must reach the owner before an in-flight business call
// completes. The owner asynchronously stops Service and only then releases
// business resources. Duplicate socket notifications must not repeat cleanup.
func TestServiceFailureShutdown(t *testing.T) {
	for _, failure := range []string{"master", "xshard", "listener", "simultaneous", "owner"} {
		t.Run(failure, func(t *testing.T) {
			backend := &serviceBackend{entered: make(chan struct{}), release: make(chan struct{})}
			var calls atomic.Int32
			done := make(chan struct{})
			requested := make(chan struct{})
			var a *Service
			a = newTestService(t, 1, backend, func() {
				calls.Add(1)
				close(requested)
				a.Stop()
				close(done)
			})
			b := newTestService(t, 2, &serviceBackend{}, noopShutdown)
			am := serviceMaster(t, a, nil)
			serviceMaster(t, b, nil)
			addr := b.Addr().(*net.TCPAddr)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.DialToSlave(ctx, comm.SlaveInfo{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: big.NewInt(2)}); err != nil {
				t.Fatal(err)
			}
			peer := a.xshardManager.Lookup(big.NewInt(2))
			// Keep a business request running while communication shuts down.
			// Cleanup also releases it if an assertion fails.
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(backend.release) }) }
			defer release()
			root := initializeService(t, am)
			result := make(chan error, 1)
			go func() {
				_, err := am.SendRPC(ctx, byte(wire.ClusterOpAddRootBlockRequest), &wire.AddRootBlockRequest{RootBlock: root})
				result <- err
			}()
			select {
			case <-backend.entered:
			case <-ctx.Done():
				t.Fatal("backend not entered")
			}
			switch failure {
			case "master":
				am.Close()
			case "xshard":
				peer.Close()
			case "listener":
				a.listener.Close()
			case "simultaneous":
				var closes sync.WaitGroup
				closes.Add(2)
				go func() { defer closes.Done(); am.Close() }()
				go func() { defer closes.Done(); peer.Close() }()
				closes.Wait()
			case "owner":
				a.shutdown()
			}
			select {
			case <-requested:
			case <-ctx.Done():
				t.Fatal("owner not notified while backend was still running")
			}
			select {
			case <-done:
				t.Fatal("Stop returned while backend was still running")
			default:
			}
			select {
			case err := <-result:
				if !errors.Is(err, conn.ErrConnectionClosed) {
					t.Fatalf("pending request after failure: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("pending request was not closed")
			}
			release()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("owner callback missing or reentrant Stop deadlocked")
			}
			if !a.master.IsClosed() || !peer.IsClosed() {
				t.Fatal("callback ran before connections closed")
			}
			if _, err := a.listener.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("listener still open: %v", err)
			}
			a.shutdown()
			a.Stop()
			if calls.Load() != 1 {
				t.Fatalf("owner shutdown calls: %d", calls.Load())
			}
		})
	}
}

func TestServiceWithShard(t *testing.T) {
	chainID := big.NewInt(10001)
	config := params.NewTestQuarkChainConfig(10001, 10001)
	s, err := NewService(ServiceConfig{
		ListenAddr: "127.0.0.1:0", LocalChainID: chainID,
		ConfiguredChainIDs: []*big.Int{chainID}, Shutdown: noopShutdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	alloc := core.SystemContractAllocs()
	maps.Copy(alloc, core.PoSWStakingContractAllocs(config))
	backend, err := shard.New(config, &core.Genesis{
		GasLimit: 12_000_000, Difficulty: new(big.Int), Mixhash: common.BigToHash(common.Big1),
		ExtraData: (&types.QKCExtra{
			ChainID: chainID, Cursor: types.XShardCursor{RootBlockHeight: 1},
			XShardGasLimit: 12_000_000 / params.XShardGasLimitDivisor,
		}).Encode(), Alloc: alloc,
	}, db, nil, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Stop() })
	t.Cleanup(func() { s.Stop() })
	if err := s.Start(backend); err != nil {
		t.Fatal(err)
	}
	remote := serviceMaster(t, s, nil)
	if backend.RootTip() != (common.Hash{}) {
		t.Fatal("identity PING initialized shard")
	}
	root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1),
	}, qkctypes.ShardBlockHeaders{backend.CurrentBlock()}, nil)
	encoded, err := root.MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	serviceRPC(t, remote, wire.ClusterOpPing, &wire.PingRequest{HasRootTip: true, RootTip: encoded})
	if backend.RootTip() != root.Hash() {
		t.Fatal("root PING did not initialize real shard")
	}
	headers := serviceRPC(t, remote, wire.ClusterOpGetUnconfirmedHeadersRequest, &wire.GetUnconfirmedHeadersRequest{}).(*wire.GetUnconfirmedHeadersResponse)
	if headers.ErrorCode != 0 || len(headers.HeaderList) != 0 {
		t.Fatalf("genesis headers: %+v", headers)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var remoteErr *comm.RemoteError
	if err := s.SendShardBlockHeaderToMaster(ctx, backend.CurrentBlock(), nil); !errors.As(err, &remoteErr) || remoteErr.Code != 9 {
		t.Fatalf("real shard header report: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.GetUnconfirmedHeaderList(); err != nil {
		t.Fatalf("Service.Stop closed borrowed shard: %v", err)
	}
}
