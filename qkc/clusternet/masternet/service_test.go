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
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/slavenet"
	"github.com/ethereum/go-ethereum/qkc/master"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

var _ master.ClusterClient = (*Service)(nil)

func noopShutdown() {}

type serviceBackend struct {
	root    *qkctypes.RootBlock
	rootErr error
	report  func(*big.Int, *types.Header, *uint256.Int) error
}

func (b *serviceBackend) RootTip() (*qkctypes.RootBlock, error) { return b.root, b.rootErr }
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
	deposits    chan common.Hash
}

func (b *serviceShard) Init(root *qkctypes.RootBlock) error {
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
func (b *serviceShard) AddXshardTxList(hash common.Hash, _ []*types.DepositTx) error {
	b.deposits <- hash
	return nil
}

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
	cfg := ServiceConfig{ConfiguredChainIDs: ids, StartupTimeout: 5 * time.Second, Shutdown: noopShutdown}
	for i := range slaves {
		b := &serviceShard{id: int64(i + 1), deposits: make(chan common.Hash, n)}
		s, err := slavenet.NewService(slavenet.ServiceConfig{ListenAddr: "127.0.0.1:0", LocalChainID: ids[i], ConfiguredChainIDs: ids, Shutdown: noopShutdown})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(b); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Stop() })
		addr := s.Addr().(*net.TCPAddr)
		cfg.Slaves = append(cfg.Slaves, clusternet.SlaveInfo{Host: "127.0.0.1", Port: uint16(addr.Port), ChainID: ids[i]})
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

func TestServiceCluster(t *testing.T) {
	for _, n := range []int{2, 8} {
		t.Run(big.NewInt(int64(n)).String(), func(t *testing.T) {
			slaves, shards, cfg := serviceCluster(t, n)
			var notifications atomic.Int32
			cfg.Shutdown = func() { notifications.Add(1) }
			s := newServiceForTest(t, cfg)
			reports := make(chan int64, n)
			backend := &serviceBackend{root: testRoot(3), report: func(id *big.Int, h *types.Header, amount *uint256.Int) error {
				if h.Number.Uint64() != 7 || amount.Uint64() != 9 {
					return errors.New("incorrect report payload")
				}
				reports <- id.Int64()
				return &clusternet.RemoteError{Code: 7}
			}}
			if err := s.BroadcastRootBlock(slaveTestContext(t), testRoot(4)); err == nil {
				t.Fatal("business RPC allowed before Start")
			}
			if err := s.Start(backend); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(backend); err == nil {
				t.Fatal("started twice")
			}
			for _, shard := range shards {
				if shard.initialized.Load() != 1 || shard.checkpoint != backend.root.Hash() {
					t.Fatal("incorrect shard checkpoint initialization")
				}
			}
			ctx := slaveTestContext(t)
			// The sequential topology phase must establish routing in both directions.
			for i, slave := range slaves {
				target := (i + 1) % n
				hash := common.Hash{byte(i + 1)}
				if err := slave.BroadcastXShardTxList(ctx, big.NewInt(int64(target+1)), hash, nil); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-shards[target].deposits:
					if got != hash {
						t.Fatal("wrong xshard hash")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			root := testRoot(4)
			if err := s.BroadcastRootBlock(ctx, root); err != nil {
				t.Fatal(err)
			}
			for _, shard := range shards {
				shard.mu.Lock()
				got := append([]common.Hash(nil), shard.roots...)
				shard.mu.Unlock()
				if len(got) != 1 || got[0] != root.Hash() {
					t.Fatal("root broadcast omitted a slave")
				}
			}
			headers, err := s.GetUnconfirmedHeaderList(ctx)
			if err != nil || len(headers) != n {
				t.Fatalf("headers: %v, %v", headers, err)
			}
			for i, slave := range slaves {
				id := big.NewInt(int64(i + 1))
				if len(headers[id.String()]) != 1 || headers[id.String()][0].Number.Cmp(id) != 0 {
					t.Fatal("incorrect header routing")
				}
				result, err := s.GetRootChainStakes(ctx, id, common.Address{}, common.Hash{})
				if err != nil || result.Stakes.Uint64() != uint64(i+1) {
					t.Fatalf("stakes: %v, %v", result, err)
				}
				var remoteErr *clusternet.RemoteError
				if err := slave.SendShardBlockHeaderToMaster(ctx, testHeader(), uint256.NewInt(9)); !errors.As(err, &remoteErr) || remoteErr.Code != 7 {
					t.Fatalf("report rejection: %v", err)
				}
			}
			for i := 0; i < n; i++ {
				select {
				case <-reports:
				case <-ctx.Done():
					t.Fatal("missing report")
				}
			}
			if _, err := s.GetRootChainStakes(ctx, big.NewInt(100), common.Address{}, common.Hash{}); err == nil {
				t.Fatal("unknown chain accepted")
			}
			s.Stop()
			s.shutdown() // late socket notifications must be harmless
			if notifications.Load() != 0 {
				t.Fatal("normal Stop requested Shutdown")
			}
			if err := s.BroadcastRootBlock(ctx, root); err == nil {
				t.Fatal("business RPC allowed after Stop")
			}
			if err := s.Start(backend); err == nil {
				t.Fatal("stopped service restarted")
			}
		})
	}
}

func TestServiceConfig(t *testing.T) {
	base := func() ServiceConfig {
		return ServiceConfig{Slaves: []clusternet.SlaveInfo{{Host: "localhost", Port: 1234, ChainID: big.NewInt(1)}}, ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, Shutdown: noopShutdown}
	}
	for _, change := range []func(*ServiceConfig){
		func(c *ServiceConfig) { c.Shutdown = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs[0] = nil },
		func(c *ServiceConfig) { c.ConfiguredChainIDs[0] = new(big.Int).Lsh(big.NewInt(1), 256) },
		func(c *ServiceConfig) { c.ConfiguredChainIDs = append(c.ConfiguredChainIDs, big.NewInt(1)) },
		func(c *ServiceConfig) { c.Slaves = nil },
		func(c *ServiceConfig) { c.Slaves[0].ChainID = nil },
		func(c *ServiceConfig) { c.Slaves[0].ChainID = big.NewInt(2) },
		func(c *ServiceConfig) { c.Slaves = append(c.Slaves, c.Slaves[0]) },
		func(c *ServiceConfig) { c.Slaves[0].Host = "" },
		func(c *ServiceConfig) { c.Slaves[0].Port = 0 },
		func(c *ServiceConfig) { c.StartupTimeout = -time.Second },
		func(c *ServiceConfig) { c.MaxPayloadSize = ^uint32(0) },
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
	if err := s.Start(nil); err == nil {
		t.Fatal("nil backend accepted")
	}
	s.Stop()
	if err := s.Start(&serviceBackend{root: testRoot(1)}); err == nil {
		t.Fatal("Start after Stop accepted")
	}
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
				cfg.Slaves = append(cfg.Slaves, clusternet.SlaveInfo{Host: "localhost", Port: 1234, ChainID: big.NewInt(id)})
			}
			_, err := NewService(cfg)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t, error=%v", test.valid, err)
			}
		})
	}
}

func TestServiceStartupFailure(t *testing.T) {
	for _, failure := range []string{"identity", "init", "checkpoint", "nil checkpoint", "dial"} {
		t.Run(failure, func(t *testing.T) {
			_, shards, cfg := serviceCluster(t, 2)
			var calls atomic.Int32
			cfg.Shutdown = func() { calls.Add(1) }
			backend := &serviceBackend{root: testRoot(1)}
			switch failure {
			case "identity":
				cfg.Slaves[0].ChainID, cfg.Slaves[1].ChainID = cfg.Slaves[1].ChainID, cfg.Slaves[0].ChainID
			case "init":
				shards[0].mu.Lock()
				shards[0].initErr = errors.New("invalid checkpoint")
				shards[0].mu.Unlock()
			case "checkpoint":
				backend.rootErr = errors.New("checkpoint unavailable")
			case "nil checkpoint":
				backend.root = nil
			case "dial":
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				cfg.Slaves[0].Port = uint16(listener.Addr().(*net.TCPAddr).Port)
				listener.Close()
			}
			s := newServiceForTest(t, cfg)
			if err := s.Start(backend); err == nil {
				t.Fatal("startup unexpectedly succeeded")
			}
			for _, sc := range s.connections() {
				if !sc.IsClosed() {
					t.Fatal("failed startup leaked connection")
				}
			}
			s.shutdown()
			if calls.Load() != 0 {
				t.Fatal("failed startup notified Shutdown")
			}
			if err := s.Start(backend); err == nil {
				t.Fatal("retried failed startup")
			}
		})
	}
}

func TestServiceBusinessRejection(t *testing.T) {
	_, shards, cfg := serviceCluster(t, 2)
	var calls atomic.Int32
	cfg.Shutdown = func() { calls.Add(1) }
	s := newServiceForTest(t, cfg)
	if err := s.Start(&serviceBackend{root: testRoot(1)}); err != nil {
		t.Fatal(err)
	}
	ctx := slaveTestContext(t)
	shards[0].mu.Lock()
	shards[0].rootErr = &clusternet.RemoteError{Code: 9}
	shards[0].headersErr = &clusternet.RemoteError{Code: 8}
	shards[0].mu.Unlock()
	var remoteErr *clusternet.RemoteError
	if err := s.BroadcastRootBlock(ctx, testRoot(2)); !errors.As(err, &remoteErr) || remoteErr.Code != 9 {
		t.Fatalf("broadcast rejection: %v", err)
	}
	if headers, err := s.GetUnconfirmedHeaderList(ctx); !errors.As(err, &remoteErr) || remoteErr.Code != 8 || headers != nil {
		t.Fatalf("partial headers: %v, %v", headers, err)
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
	if err := s.Start(&serviceBackend{root: testRoot(1)}); err != nil {
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
			if err := s.Start(&serviceBackend{root: testRoot(1), report: func(*big.Int, *types.Header, *uint256.Int) error { close(entered); <-release; return nil }}); err != nil {
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

func TestServiceStartupTimeout(t *testing.T) {
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
	cfg := ServiceConfig{Slaves: []clusternet.SlaveInfo{{Host: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port), ChainID: big.NewInt(1)}}, ConfiguredChainIDs: []*big.Int{big.NewInt(1)}, StartupTimeout: 100 * time.Millisecond, Shutdown: noopShutdown}
	s := newServiceForTest(t, cfg)
	if err := s.Start(&serviceBackend{root: testRoot(1)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup timeout: %v", err)
	}
	select {
	case nc := <-accepted:
		defer nc.Close()
	case <-time.After(time.Second):
		t.Fatal("slave not dialed")
	}
	for _, sc := range s.connections() {
		if !sc.IsClosed() {
			t.Fatal("timed-out connection left open")
		}
	}
}

func TestServiceRequiresAllConfiguredConnections(t *testing.T) {
	_, _, cfg := serviceCluster(t, 2)
	s := newServiceForTest(t, cfg)
	if err := s.Start(&serviceBackend{root: testRoot(1)}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	missing := s.conns["2"]
	delete(s.conns, "2")
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.conns["2"] = missing; s.mu.Unlock() }()
	for _, call := range []func() error{
		func() error { return s.BroadcastRootBlock(slaveTestContext(t), testRoot(2)) },
		func() error { _, err := s.GetUnconfirmedHeaderList(slaveTestContext(t)); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "no slave connection") {
			t.Fatalf("missing connection omitted: %v", err)
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
	backend := &serviceShard{id: 2, deposits: make(chan common.Hash, 1)}
	if err := second.Start(backend); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Stop() })
	cfg.ConfiguredChainIDs = ids
	cfg.Slaves = append(cfg.Slaves, clusternet.SlaveInfo{Host: "127.0.0.1", Port: uint16(second.Addr().(*net.TCPAddr).Port), ChainID: ids[1]})
	var calls atomic.Int32
	cfg.Shutdown = func() { calls.Add(1) }
	s := newServiceForTest(t, cfg)
	if err := s.Start(&serviceBackend{root: testRoot(1)}); err == nil || !strings.Contains(err.Error(), "interconnect") {
		t.Fatalf("interconnection failure: %v", err)
	}
	if shards[0].initialized.Load() != 0 || backend.initialized.Load() != 0 {
		t.Fatal("initialized shards after failed interconnection")
	}
	for _, sc := range s.connections() {
		if !sc.IsClosed() {
			t.Fatal("failed interconnection left a slave open")
		}
	}
	s.shutdown()
	if calls.Load() != 0 {
		t.Fatal("failed interconnection requested shutdown")
	}
}

func TestServiceUint256Values(t *testing.T) {
	maxID := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	ids := []*big.Int{new(big.Int).Lsh(big.NewInt(1), 128), maxID}
	slaves, _, cfg := serviceClusterWithIDs(t, ids)
	s := newServiceForTest(t, cfg)
	amount := new(uint256.Int).SetAllOne()
	reports := make(chan *big.Int, len(ids))
	if err := s.Start(&serviceBackend{root: testRoot(1), report: func(id *big.Int, _ *types.Header, got *uint256.Int) error {
		if !got.Eq(amount) {
			return errors.New("truncated coinbase amount")
		}
		reports <- id
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	ctx := slaveTestContext(t)
	headers, err := s.GetUnconfirmedHeaderList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if len(headers[id.String()]) != 1 {
			t.Fatalf("missing headers for chain %s", id)
		}
		result, err := s.GetRootChainStakes(ctx, new(big.Int).Set(id), common.Address{}, common.Hash{})
		if err != nil || result.Stakes.Uint64() != uint64(i+1) {
			t.Fatalf("stakes routing: %v, %v", result, err)
		}
		if err := slaves[i].SendShardBlockHeaderToMaster(ctx, testHeader(), amount); err != nil {
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
		if err := slaves[i].BroadcastXShardTxList(ctx, ids[1-i], common.Hash{}, nil); err != nil {
			t.Fatal(err)
		}
	}
}
