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

package masternet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet/slavenet"
	"github.com/ethereum/go-ethereum/qkc/net/conn"
	"github.com/ethereum/go-ethereum/qkc/net/wire"
	"github.com/ethereum/go-ethereum/qkc/root"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

var _ root.ClusterClient = (*Service)(nil)

// startAndWait starts a fresh service once and waits for initialization.
func startAndWait(t *testing.T, s *Service, backend MasterBackend) error {
	t.Helper()
	if err := s.Start(backend); err != nil {
		return err
	}
	return waitForService(t, s)
}

func waitForService(t *testing.T, s *Service) error {
	t.Helper()
	ctx := slaveTestContext(t)
	select {
	case <-s.WaitForInitialization():
		if !s.IsActive() {
			return errors.New("master service is not active")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func noopShutdown() {}

type serviceBackend struct {
	report func(*big.Int, *types.Header, *uint256.Int) error
}

func (b *serviceBackend) AddShardBlockHeader(id *big.Int, h *types.Header, amount *uint256.Int) error {
	if b.report != nil {
		return b.report(id, h, amount)
	}
	return nil
}

type serviceShard struct {
	id          int64
	initialized atomic.Int32
	mu          sync.Mutex
	checkpoint  common.Hash
	roots       []common.Hash
	rootErr     error
	headersErr  error
	initErr     error
	initEntered chan struct{}
	initRelease <-chan struct{}
}

func (b *serviceShard) Init(root *qkctypes.RootBlock) error {
	if b.initEntered != nil {
		close(b.initEntered)
		<-b.initRelease
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checkpoint = root.Hash()
	b.initialized.Add(1)
	return b.initErr
}
func (b *serviceShard) AddRootBlock(root *qkctypes.RootBlock) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roots = append(b.roots, root.Hash())
	return true, b.rootErr
}
func (b *serviceShard) GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error) {
	return uint256.NewInt(uint64(b.id)), common.Address{byte(b.id)}, nil
}
func (b *serviceShard) GetUnconfirmedHeaderList() ([]*types.Header, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return []*types.Header{{Number: big.NewInt(b.id), Difficulty: big.NewInt(1)}}, b.headersErr
}
func (b *serviceShard) AddXshardTxList(common.Hash, []*types.DepositTx) error { return nil }

func serviceCluster(t *testing.T, n int) ([]*slavenet.Service, []*serviceShard, ServiceConfig) {
	t.Helper()
	ids := make([]*big.Int, n)
	for i := range ids {
		ids[i] = big.NewInt(int64(i + 1))
	}
	return serviceClusterWithIDs(t, ids)
}

func serviceClusterWithIDs(t *testing.T, ids []*big.Int) ([]*slavenet.Service, []*serviceShard, ServiceConfig) {
	t.Helper()
	n := len(ids)
	slaves := make([]*slavenet.Service, n)
	backends := make([]*serviceShard, n)
	cfg := ServiceConfig{ConfiguredChainIDs: ids, Shutdown: noopShutdown}
	for i := range slaves {
		b := &serviceShard{id: int64(i + 1)}
		s, err := slavenet.NewService(slavenet.ServiceConfig{ListenAddr: "127.0.0.1:0", LocalChainID: ids[i], ConfiguredChainIDs: ids, Shutdown: noopShutdown})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(b); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Stop() })
		addr := s.Addr().(*net.TCPAddr)
		cfg.SlaveInfos = append(cfg.SlaveInfos, clusternet.SlaveInfo{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: ids[i]})
		slaves[i], backends[i] = s, b
	}
	return slaves, backends, cfg
}
func newServiceForTest(t *testing.T, cfg ServiceConfig) *Service {
	t.Helper()
	s, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestServiceConfig(t *testing.T) {
	base := func() ServiceConfig {
		return ServiceConfig{SlaveInfos: []clusternet.SlaveInfo{{Host: "localhost", Port: 1234, ChainID: big.NewInt(1)}}, ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown}
	}
	for _, change := range []func(*ServiceConfig){
		func(c *ServiceConfig) { c.Shutdown = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs[0] = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs[0] = new(big.Int).Lsh(big.NewInt(1), 256) },
		func(c *ServiceConfig) { c.ConfiguredChainIDs = append(c.ConfiguredChainIDs, big.NewInt(1)) },
		func(c *ServiceConfig) { c.SlaveInfos = nil },
		func(c *ServiceConfig) { c.SlaveInfos[0].ChainID = nil },
		func(c *ServiceConfig) { c.SlaveInfos[0].ChainID = big.NewInt(2) },
		func(c *ServiceConfig) { c.SlaveInfos = append(c.SlaveInfos, c.SlaveInfos[0]) },
		func(c *ServiceConfig) { c.SlaveInfos[0].Host = "" },
		func(c *ServiceConfig) { c.SlaveInfos[0].Port = 0 },
	} {
		cfg := base()
		change(&cfg)
		if _, err := NewService(cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	cfg := base()
	s, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := startAndWait(t, s, nil); err == nil {
		t.Fatal("nil backend accepted")
	}
	s.Stop()
}

func TestServiceChainMapping(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured []int64
		slaves     []int64
		valid      bool
	}{
		{"reordered", []int64{1, 2}, []int64{2, 1}, true},
		{"duplicate configured", []int64{1, 1}, []int64{1, 2}, false},
		{"duplicate slaves", []int64{1, 2}, []int64{1, 1}, false},
		{"duplicate both", []int64{1, 1}, []int64{1, 1}, false},
		{"different IDs", []int64{1, 2}, []int64{1, 3}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := ServiceConfig{Shutdown: noopShutdown}
			for _, id := range test.configured {
				cfg.ConfiguredChainIDs = append(cfg.ConfiguredChainIDs, big.NewInt(id))
			}
			for _, id := range test.slaves {
				cfg.SlaveInfos = append(cfg.SlaveInfos, clusternet.SlaveInfo{Host: "localhost", Port: 1234, ChainID: big.NewInt(id)})
			}
			_, err := NewService(cfg)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t, error=%v", test.valid, err)
			}
		})
	}
}

func TestServiceStartupFailure(t *testing.T) {
	_, _, cfg := serviceCluster(t, 2)
	var calls atomic.Int32
	requested := make(chan struct{})
	var s *Service
	cfg.Shutdown = func() {
		calls.Add(1)
		s.Stop()
		close(requested)
	}
	cfg.SlaveInfos[0].ChainID, cfg.SlaveInfos[1].ChainID = cfg.SlaveInfos[1].ChainID, cfg.SlaveInfos[0].ChainID
	s = newServiceForTest(t, cfg)
	if err := startAndWait(t, s, &serviceBackend{}); err == nil {
		t.Fatal("startup unexpectedly succeeded")
	}
	select {
	case <-requested:
	case <-slaveTestContext(t).Done():
		t.Fatal("initialization failure did not notify owner")
	}
	s.Stop()
	for _, sc := range s.connections() {
		if !sc.IsClosed() {
			t.Fatal("failed startup leaked connection")
		}
	}
	s.shutdown()
	if calls.Load() != 1 {
		t.Fatal("failed startup did not notify Shutdown once")
	}
}

func TestServicePingFailure(t *testing.T) {
	_, shards, cfg := serviceCluster(t, 2)
	shards[0].initErr = errors.New("invalid checkpoint")
	s := newServiceForTest(t, cfg)
	if err := startAndWait(t, s, &serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(slaveTestContext(t), testRoot(1)); err == nil {
		t.Fatal("failed shard initialization accepted")
	}
	if shards[1].initialized.Load() != 1 {
		t.Fatal("Ping did not wait for the other shard after partial failure")
	}
}

func TestServiceBusinessRejection(t *testing.T) {
	slaves, shards, cfg := serviceCluster(t, 2)
	var calls atomic.Int32
	cfg.Shutdown = func() { calls.Add(1) }
	s := newServiceForTest(t, cfg)
	backend := &serviceBackend{report: func(*big.Int, *types.Header, *uint256.Int) error {
		return &wire.RemoteError{Code: 7}
	}}
	if err := startAndWait(t, s, backend); err != nil {
		t.Fatal(err)
	}
	ctx := slaveTestContext(t)
	shards[0].mu.Lock()
	shards[0].rootErr = &wire.RemoteError{Code: 9}
	shards[0].headersErr = &wire.RemoteError{Code: 8}
	shards[0].mu.Unlock()
	var remoteErr *wire.RemoteError
	if err := s.BroadcastRootBlock(ctx, testRoot(2)); !errors.As(err, &remoteErr) || remoteErr.Code != 9 {
		t.Fatalf("broadcast rejection: %v", err)
	}
	if headers, err := s.GetUnconfirmedHeaderList(ctx); !errors.As(err, &remoteErr) || remoteErr.Code != 8 || headers != nil {
		t.Fatalf("partial headers: %v, %v", headers, err)
	}
	if err := slaves[0].SendShardBlockHeaderToMaster(ctx, testHeader(), uint256.NewInt(9)); !errors.As(err, &remoteErr) || remoteErr.Code != 7 {
		t.Fatalf("report rejection: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.BroadcastRootBlock(canceled, testRoot(2)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled broadcast: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("business rejection requested shutdown")
	}
	// A subsequent query proves rejection did not close the connection.
	if _, err := s.GetRootChainStakes(ctx, big.NewInt(1), common.Address{}, common.Hash{}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceUnexpectedDisconnect(t *testing.T) {
	slaves, _, cfg := serviceCluster(t, 2)
	var calls atomic.Int32
	done := make(chan struct{})
	var s *Service
	cfg.Shutdown = func() { calls.Add(1); go func() { s.Stop(); close(done) }() }
	s = newServiceForTest(t, cfg)
	if err := startAndWait(t, s, &serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	var stopped sync.WaitGroup
	for _, slave := range slaves {
		stopped.Add(1)
		go func() { defer stopped.Done(); slave.Stop() }()
	}
	stopped.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect failed to stop master")
	}
	s.shutdown()
	if calls.Load() != 1 {
		t.Fatalf("shutdown calls: %d", calls.Load())
	}
}

func TestServiceStopDrainsReports(t *testing.T) {
	for _, passive := range []bool{false, true} {
		t.Run(fmt.Sprintf("passive=%t", passive), func(t *testing.T) {
			slaves, _, cfg := serviceCluster(t, 2)
			var calls atomic.Int32
			requested := make(chan struct{})
			stopped := make(chan struct{})
			var s *Service
			cfg.Shutdown = func() {
				calls.Add(1)
				close(requested)
				go func() { s.Stop(); close(stopped) }()
			}
			s = newServiceForTest(t, cfg)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			if err := startAndWait(t, s, &serviceBackend{report: func(*big.Int, *types.Header, *uint256.Int) error { close(entered); <-release; return nil }}); err != nil {
				t.Fatal(err)
			}
			ctx := slaveTestContext(t)
			result := make(chan error, 1)
			go func() { result <- slaves[0].SendShardBlockHeaderToMaster(ctx, testHeader(), uint256.NewInt(9)) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("report not entered")
			}
			if passive {
				s.connection(big.NewInt(1)).Close()
				select {
				case <-requested:
				case <-ctx.Done():
					t.Fatal("disconnect did not notify owner while backend was running")
				}
			} else {
				go func() { s.Stop(); close(stopped) }()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("pending report survived close")
				}
			case <-ctx.Done():
				t.Fatal("pending report not released")
			}
			select {
			case <-stopped:
				t.Fatal("Stop returned with backend running")
			default:
			}
			unblock()
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("Stop did not drain backend")
			}
			s.shutdown()
			want := int32(0)
			if passive {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("shutdown calls: got %d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestServiceStartupRPCContext(t *testing.T) {
	t.Parallel()
	const parentTimeout = 200 * time.Millisecond
	for _, operation := range []struct {
		name     string
		opcode   wire.ClusterOp
		send     func(*Service, context.Context) error
		response wire.SSZMessage
	}{
		{"identity", wire.ClusterOpPing, (*Service).identifySlaves, &wire.PongResponse{ChainID: *uint256.NewInt(1)}},
		{"interconnect", wire.ClusterOpConnectToSlavesRequest, (*Service).interconnectSlaves, &wire.ConnectToSlavesResponse{ResultList: [][]byte{nil}}},
	} {
		for _, test := range []struct {
			name          string
			parentTimeout time.Duration
			wantElapsed   time.Duration
			wantErr       error
		}{
			{"parent-deadline", parentTimeout, parentTimeout, context.DeadlineExceeded},
			{"parent-cancellation", 0, 0, context.Canceled},
			{"startup-deadline", 0, startupNetworkTimeout, context.DeadlineExceeded},
		} {
			t.Run(operation.name+"/"+test.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					defer close(release)
					sc, _ := newSlaveTestPair(t, big.NewInt(1), nil, map[byte]conn.TypedHandler{
						byte(operation.opcode): func(wire.SSZMessage) (wire.SSZMessage, error) {
							close(entered)
							<-release
							return operation.response, nil
						},
					})
					service := newServiceForTest(t, ServiceConfig{
						SlaveInfos:         []clusternet.SlaveInfo{{Host: "127.0.0.1", Port: 1, ChainID: big.NewInt(1)}},
						ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown,
					})
					service.conns["1"] = sc
					start := time.Now()
					var ctx context.Context
					var cancel context.CancelFunc
					if test.parentTimeout > 0 {
						ctx, cancel = context.WithTimeout(t.Context(), test.parentTimeout)
					} else {
						ctx, cancel = context.WithCancel(t.Context())
					}
					defer cancel()
					timeout := time.After(startupNetworkTimeout + time.Second)
					result := make(chan error, 1)
					go func() { result <- operation.send(service, ctx) }()
					select {
					case <-entered:
					case <-timeout:
						t.Fatal("startup RPC did not reach the slave")
					}
					if test.wantErr == context.Canceled {
						cancel()
					}
					select {
					case err := <-result:
						if elapsed := time.Since(start); !errors.Is(err, test.wantErr) || elapsed != test.wantElapsed {
							t.Fatalf("startup RPC returned %v after %s, want %v after %s", err, elapsed, test.wantErr, test.wantElapsed)
						}
					case <-timeout:
						t.Fatal("startup RPC did not return after cancellation or deadline")
					}
				})
			})
		}
	}
}

func TestServiceInterconnectionFailure(t *testing.T) {
	// Slave 1 accepts its master but does not allow the advertised remote chain.
	_, shards, cfg := serviceCluster(t, 1)
	ids := []*big.Int{big.NewInt(1), big.NewInt(2)}
	second, err := slavenet.NewService(slavenet.ServiceConfig{
		ListenAddr: "127.0.0.1:0", LocalChainID: ids[1], ConfiguredChainIDs: ids, Shutdown: noopShutdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &serviceShard{id: 2}
	if err := second.Start(backend); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Stop() })
	cfg.ConfiguredChainIDs = ids
	cfg.SlaveInfos = append(cfg.SlaveInfos, clusternet.SlaveInfo{Host: "127.0.0.1", Port: uint16(second.Addr().(*net.TCPAddr).Port), ChainID: ids[1]})
	var calls atomic.Int32
	requested := make(chan struct{})
	cfg.Shutdown = func() { calls.Add(1); close(requested) }
	s := newServiceForTest(t, cfg)
	if err := startAndWait(t, s, &serviceBackend{}); err == nil {
		t.Fatalf("interconnection failure: %v", err)
	}
	if shards[0].initialized.Load() != 0 || backend.initialized.Load() != 0 {
		t.Fatal("initialized shards after failed interconnection")
	}
	select {
	case <-requested:
	case <-slaveTestContext(t).Done():
		t.Fatal("interconnection failure did not notify owner")
	}
	s.Stop()
	for _, sc := range s.connections() {
		if !sc.IsClosed() {
			t.Fatal("failed interconnection left a slave open")
		}
	}
	s.shutdown()
	if calls.Load() != 1 {
		t.Fatal("failed interconnection did not request shutdown once")
	}
}

func TestServiceUint256Values(t *testing.T) {
	maxID := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	ids := []*big.Int{new(big.Int).Lsh(big.NewInt(1), 128), maxID}
	slaves, _, cfg := serviceClusterWithIDs(t, ids)
	s := newServiceForTest(t, cfg)
	amount := new(uint256.Int).SetAllOne()
	header := testHeader()
	reports := make(chan *big.Int, len(ids))
	if err := startAndWait(t, s, &serviceBackend{report: func(id *big.Int, h *types.Header, got *uint256.Int) error {
		if !got.Eq(amount) {
			return errors.New("truncated coinbase amount")
		}
		if h.Hash() != header.Hash() {
			return errors.New("incorrect reported header")
		}
		reports <- id
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx := slaveTestContext(t)
	headers, err := s.GetUnconfirmedHeaderList(ctx)
	if err != nil || len(headers) != len(ids) {
		t.Fatalf("headers: %v, %v", headers, err)
	}
	for i, id := range ids {
		list := headers[id.String()]
		if len(list) != 1 || list[0].Number.Uint64() != uint64(i+1) {
			t.Fatalf("incorrect headers for chain %s: %v", id, list)
		}
		result, err := s.GetRootChainStakes(ctx, new(big.Int).Set(id), common.Address{}, common.Hash{})
		if err != nil || result.Stakes.Uint64() != uint64(i+1) {
			t.Fatalf("stakes routing: %v, %v", result, err)
		}
		if err := slaves[i].SendShardBlockHeaderToMaster(ctx, header, amount); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-reports:
			if got.Cmp(id) != 0 {
				t.Fatalf("report source: got %s, want %s", got, id)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := s.GetRootChainStakes(ctx, big.NewInt(1), common.Address{}, common.Hash{}); err == nil {
		t.Fatal("unknown chain accepted")
	}
}

func TestServiceBroadcastUnavailable(t *testing.T) {
	for _, s := range []*Service{
		{},
		{state: serviceStateStarting, conns: map[string]*SlaveConn{"1": {}}},
		{state: serviceStateStarting},
		{state: serviceStateStopped},
	} {
		if err := s.Ping(slaveTestContext(t), testRoot(1)); err == nil {
			t.Fatal("unavailable cluster accepted Ping")
		}
		if err := s.BroadcastRootBlock(slaveTestContext(t), testRoot(1)); err == nil {
			t.Fatal("unavailable cluster accepted broadcast")
		}
		if _, err := s.GetUnconfirmedHeaderList(slaveTestContext(t)); err == nil {
			t.Fatal("unavailable cluster accepted header query")
		}
		if result, err := s.GetRootChainStakes(slaveTestContext(t), big.NewInt(1), common.Address{}, common.Hash{}); result != nil || err == nil || err.Error() != "master service is not ready" {
			t.Fatalf("unavailable cluster stakes query: result=%v, err=%v", result, err)
		}
	}
}

func TestServiceAsyncStartAndStopCancelsInitialization(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		nc, err := listener.Accept()
		if err == nil {
			accepted <- nc
		}
	}()
	var calls atomic.Int32
	s := newServiceForTest(t, ServiceConfig{
		SlaveInfos:         []clusternet.SlaveInfo{{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), ChainID: big.NewInt(1)}},
		ConfiguredChainIDs: []*big.Int{big.NewInt(1)},
		Shutdown:           func() { calls.Add(1) },
	})
	select {
	case <-s.WaitForInitialization():
		t.Fatal("unstarted service reported ready")
	default:
	}
	// The peer never answers identity PING; Start must return anyway.
	returned := make(chan error, 1)
	go func() { returned <- s.Start(&serviceBackend{}) }()
	ctx := slaveTestContext(t)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Start blocked on initialization")
	}
	select {
	case nc := <-accepted:
		defer nc.Close()
		if err := nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var buffer [1]byte
		if _, err := nc.Read(buffer[:]); err != nil {
			t.Fatalf("initialization did not send PING: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("initialization did not connect")
	}
	if s.connection(big.NewInt(1)) != nil {
		t.Fatal("initializing connection exposed to business calls")
	}
	if err := s.BroadcastRootBlock(ctx, testRoot(2)); err == nil {
		t.Fatal("initializing service accepted broadcast")
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != serviceStateStarting {
		t.Fatalf("readiness wait changed lifecycle: %v", state)
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel initialization")
	}
	if err := waitForService(t, s); err == nil {
		t.Fatalf("stopped initialization should not become active: %v", err)
	}
	s.shutdown()
	if calls.Load() != 0 {
		t.Fatal("active stop requested Shutdown")
	}
	if s.state != serviceStateStopped {
		t.Fatalf("state after Stop: %v", s.state)
	}
}

type startupWriteConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (connection *startupWriteConn) Write(buffer []byte) (int, error) {
	connection.once.Do(func() { close(connection.entered) })
	return connection.Conn.Write(buffer)
}

func TestServiceStopInterruptsStartupWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, remote := net.Pipe()
		defer local.Close()
		defer remote.Close()
		observed := &startupWriteConn{Conn: local, entered: make(chan struct{})}
		connection, err := NewSlaveConn(SlaveConnConfig{
			Conn: observed, RemoteChainID: big.NewInt(1), Handler: slaveHandlerFunc{},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		var startupFailed, shutdownRequested atomic.Bool
		// Use a preconnected pipe so the real startup path blocks on its first PING write.
		service := &Service{
			cfg: ServiceConfig{
				Logger: log.NewLogger(log.JSONHandler(serviceLogWriter(func(p []byte) (int, error) {
					if strings.Contains(string(p), "Cluster initialization failed") {
						startupFailed.Store(true)
					}
					return len(p), nil
				}))),
				Shutdown: func() { shutdownRequested.Store(true) },
			},
			conns:    map[string]*SlaveConn{"1": connection},
			initDone: make(chan struct{}),
		}
		connection.Start()
		if err := service.Start(&serviceBackend{}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-observed.entered:
		case <-time.After(time.Second):
			t.Fatal("startup did not begin writing")
		}
		stopped := make(chan struct{})
		go func() { service.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("Stop did not interrupt the startup write")
		}
		synctest.Wait()
		if startupFailed.Load() {
			t.Fatal("Stop logged a startup failure")
		}
		if shutdownRequested.Load() {
			t.Fatal("Stop requested Shutdown")
		}
		if !connection.IsClosed() || service.IsActive() {
			t.Fatal("Stop left the service or its connection active")
		}
	})
}

func TestServicePingWaitsForAllInitializationPongs(t *testing.T) {
	_, shards, cfg := serviceCluster(t, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	for _, shard := range shards {
		shard.initEntered = make(chan struct{})
		shard.initRelease = release
	}
	s := newServiceForTest(t, cfg)
	checkpoint := testRoot(1)
	if err := startAndWait(t, s, &serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	for _, shard := range shards {
		if shard.initialized.Load() != 0 {
			t.Fatal("communication startup initialized a shard")
		}
	}
	ctx := slaveTestContext(t)
	result := make(chan error, 1)
	go func() { result <- s.Ping(ctx, checkpoint) }()
	for _, shard := range shards {
		select {
		case <-shard.initEntered:
		case <-ctx.Done():
			t.Fatal("second PING did not reach every shard")
		}
	}
	select {
	case err := <-result:
		t.Fatalf("Ping returned before all initialization PONGs: %v", err)
	default:
	}
	if !s.IsActive() {
		t.Fatal("shard initialization changed communication readiness")
	}
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Ping did not finish after all initialization PONGs")
	}
	root := testRoot(2)
	if err := s.BroadcastRootBlock(ctx, root); err != nil {
		t.Fatal(err)
	}
	for _, shard := range shards {
		shard.mu.Lock()
		initialized, gotCheckpoint := shard.initialized.Load(), shard.checkpoint
		gotRoots := append([]common.Hash(nil), shard.roots...)
		shard.mu.Unlock()
		if initialized != 1 || gotCheckpoint != checkpoint.Hash() {
			t.Fatalf("slave %d initialized %d times with checkpoint %v, want once with %v", shard.id, initialized, gotCheckpoint, checkpoint.Hash())
		}
		if len(gotRoots) != 1 || gotRoots[0] != root.Hash() {
			t.Fatalf("slave %d received roots %v, want [%v]", shard.id, gotRoots, root.Hash())
		}
	}
	if _, err := s.GetUnconfirmedHeaderList(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRootChainStakes(ctx, big.NewInt(1), common.Address{}, common.Hash{}); err != nil {
		t.Fatal(err)
	}
}

type serviceLogWriter func([]byte) (int, error)

func (w serviceLogWriter) Write(p []byte) (int, error) { return w(p) }

func TestServiceDialRetry(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop=%t", stop), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().(*net.TCPAddr)
			listener.Close()
			attempts := make(chan struct{}, 1)
			var shutdowns atomic.Int32
			cfg := ServiceConfig{
				SlaveInfos:         []clusternet.SlaveInfo{{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: big.NewInt(1)}},
				ConfiguredChainIDs: []*big.Int{big.NewInt(1)},
				Shutdown:           func() { shutdowns.Add(1) },
				Logger: log.NewLogger(log.JSONHandler(serviceLogWriter(func(p []byte) (int, error) {
					if strings.Contains(string(p), "Failed to connect to slave, retrying") {
						select {
						case attempts <- struct{}{}:
						default:
						}
					}
					return len(p), nil
				}))),
			}
			s := newServiceForTest(t, cfg)
			if err := s.Start(&serviceBackend{}); err != nil {
				t.Fatal(err)
			}
			ctx := slaveTestContext(t)
			select {
			case <-attempts:
			case <-ctx.Done():
				t.Fatal("dial failure was not retried")
			}
			select {
			case <-s.initDone:
				t.Fatal("dial failure ended initialization")
			default:
			}
			if stop {
				stopped := make(chan struct{})
				go func() { s.Stop(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(500 * time.Millisecond):
					t.Fatal("Stop did not interrupt the dial retry delay")
				}
				if err := waitForService(t, s); err == nil {
					t.Fatalf("stopped dial retry: %v", err)
				}
			} else {
				// A second failure confirms initialization keeps retrying.
				select {
				case <-attempts:
				case <-ctx.Done():
					t.Fatal("dial did not retry")
				}
				slave, err := slavenet.NewService(slavenet.ServiceConfig{
					ListenAddr: addr.String(), LocalChainID: big.NewInt(1),
					ConfiguredChainIDs: cfg.ConfiguredChainIDs, Shutdown: noopShutdown,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := slave.Start(&serviceShard{id: 1}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { slave.Stop() })
				if err := waitForService(t, s); err != nil {
					t.Fatal(err)
				}
				// Stop master first so slave cleanup is not a passive failure.
				s.Stop()
			}
			if shutdowns.Load() != 0 {
				t.Fatal("initial dial failure requested shutdown")
			}
		})
	}
}

func TestServiceCanceledDial(t *testing.T) {
	cfg := ServiceConfig{
		SlaveInfos:         []clusternet.SlaveInfo{{Host: "127.0.0.1", Port: 1, ChainID: big.NewInt(1)}},
		ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown,
	}
	service := newServiceForTest(t, cfg)
	ctx, cancel := context.WithCancel(slaveTestContext(t))
	cancel()
	if err := service.connectSlave(ctx, cfg.SlaveInfos[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dial: %v", err)
	}
	if len(service.connections()) != 0 {
		t.Fatal("canceled dial registered a connection")
	}
}

func TestServiceRejectsConnectionAfterStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := ServiceConfig{
		SlaveInfos: []clusternet.SlaveInfo{{
			Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), ChainID: big.NewInt(1),
		}},
		ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown,
	}
	service := newServiceForTest(t, cfg)
	service.Stop()
	connectErr := service.connectSlave(slaveTestContext(t), cfg.SlaveInfos[0])
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if !errors.Is(connectErr, context.Canceled) {
		t.Fatalf("connection completed after Stop: %v", connectErr)
	}
	if len(service.connections()) != 0 {
		t.Fatal("registered a connection after Stop")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	if _, err := peer.Read(buffer[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("late connection was not closed: %v", err)
	}
}

func TestServiceIdentityPingWaitsForAllConnections(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secondPort := uint16(second.Addr().(*net.TCPAddr).Port)
	second.Close() // The second slave has not started yet.
	s := newServiceForTest(t, ServiceConfig{
		SlaveInfos: []clusternet.SlaveInfo{
			{Host: "127.0.0.1", Port: uint16(first.Addr().(*net.TCPAddr).Port), ChainID: big.NewInt(1)},
			{Host: "127.0.0.1", Port: secondPort, ChainID: big.NewInt(2)},
		},
		ConfiguredChainIDs: []*big.Int{big.NewInt(1), big.NewInt(2)},
		Shutdown:           noopShutdown,
	})
	if err := s.Start(&serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	if err := first.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	nc, err := first.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := nc.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	n, err := nc.Read(buf[:])
	var timeout net.Error
	if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("sent identity PING before all slaves connected: n=%d, err=%v", n, err)
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-slaveTestContext(t).Done():
		t.Fatal("Stop did not end connection preparation")
	}
}

func TestServicePingUnavailable(t *testing.T) {
	_, shards, cfg := serviceCluster(t, 1)
	s := newServiceForTest(t, cfg)
	ctx := slaveTestContext(t)
	if err := s.Ping(ctx, testRoot(1)); err == nil {
		t.Fatal("unstarted service accepted Ping")
	}
	if err := startAndWait(t, s, &serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx, nil); err == nil {
		t.Fatal("nil checkpoint accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Ping(canceled, testRoot(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Ping: %v", err)
	}
	s.Stop()
	if err := s.Ping(ctx, testRoot(1)); err == nil {
		t.Fatal("stopped service accepted Ping")
	}
	if shards[0].initialized.Load() != 0 {
		t.Fatal("unavailable Ping initialized a shard")
	}
}

func TestServiceDisconnectDuringInitializationRequestsOwnerStop(t *testing.T) {
	slaves, shards, cfg := serviceCluster(t, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	shards[1].initEntered = make(chan struct{})
	shards[1].initRelease = release
	requested := make(chan struct{})
	stopped := make(chan struct{})
	var calls atomic.Int32
	var s *Service
	cfg.Shutdown = func() {
		calls.Add(1)
		close(requested)
		go func() { s.Stop(); close(stopped) }()
	}
	s = newServiceForTest(t, cfg)
	if err := startAndWait(t, s, &serviceBackend{}); err != nil {
		t.Fatal(err)
	}
	ctx := slaveTestContext(t)
	result := make(chan error, 1)
	go func() { result <- s.Ping(ctx, testRoot(1)) }()
	select {
	case <-shards[1].initEntered:
	case <-ctx.Done():
		t.Fatal("second PING did not reach shard")
	}
	// A disconnect requests owner shutdown even while another shard is initializing.
	slaves[0].Stop()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("disconnect did not request Shutdown")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("owner Stop did not finish while Ping was in flight")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Ping succeeded after owner Stop")
		}
	case <-ctx.Done():
		t.Fatal("Stop did not interrupt Ping")
	}
	unblock()
	if s.state != serviceStateStopped {
		t.Fatal("service was not stopped by owner")
	}
	for _, sc := range s.connections() {
		if !sc.IsClosed() {
			t.Fatal("owner Stop left a slave connection open")
		}
	}
	s.shutdown()
	if calls.Load() != 1 {
		t.Fatalf("shutdown calls: got %d, want 1", calls.Load())
	}
}
