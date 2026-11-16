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
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// MasterBackend supplies concurrent-safe business operations borrowed by Service.
// Business rejections use *clusternet.RemoteError; other handler errors close the connection.
type MasterBackend interface {
	// RootTip returns the recovered checkpoint used to initialize all shards.
	// It must return promptly; communication timeouts do not interrupt backend calls.
	RootTip() (*qkctypes.RootBlock, error)
	AddShardBlockHeader(sourceChainID *big.Int, header *types.Header, coinbaseAmount *uint256.Int) error
}

// ServiceState is the master communication service lifecycle.
type ServiceState uint8

const (
	ServiceStateCreated ServiceState = iota
	ServiceStateStarting
	ServiceStateActive
	ServiceStateStopped
)

// Service owns master-to-slave connections, not root-chain or mining resources.
// Assembly is NewService -> master.New(client=Service) -> Start(master).
// Start launches cluster initialization; WaitUntilReady waits for identity
// checks, slave interconnection, and shard initialization. The owner then recovers
// unfinished root broadcasts before starting normal business activity. Startup is attempted once;
// failures request node shutdown; the owner calls Stop to drain connections.
// Initial TCP dials retry until connected or stopped. Established connections
// are never reconnected.
type Service struct {
	cfg     ServiceConfig
	backend MasterBackend

	mu           sync.Mutex            // protects lifecycle and connection ownership
	conns        map[string]*SlaveConn // includes connections still verifying identity
	state        ServiceState
	initDone     chan struct{}
	shutdownOnce sync.Once
	stopOnce     sync.Once
}

var _ SlaveRequestHandler = (*Service)(nil)

// -- Public lifecycle methods ------------------------------------------------

// NewService validates configuration without opening connections.
// The caller must not modify configuration slices or chain IDs afterward.
func NewService(cfg ServiceConfig) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, conns: make(map[string]*SlaveConn), initDone: make(chan struct{})}, nil
}

// Start installs the backend and launches cluster initialization without waiting.
// The owner calls Start and Stop serially. Initialization failure requests node
// shutdown; restart requires a new service.
func (s *Service) Start(backend MasterBackend) error {
	if s.state != ServiceStateCreated {
		return errors.New("master service already started or stopped")
	}
	if backend == nil {
		return errors.New("missing master backend")
	}
	s.backend, s.state = backend, ServiceStateStarting
	go func() {
		err := s.initCluster()
		close(s.initDone)
		if err != nil {
			s.cfg.Logger.Debug("Cluster initialization failed", "err", err)
			s.shutdown()
		}
	}()
	return nil
}

// Stop ends dial retries, waits for initialization, closes connections, and drains
// admitted handlers. Initialization operations finish or reach their own timeout.
// It does not cancel backend calls or stop backend resources.
// The owner may release master resources after Stop returns. Normal Stop does
// not request Shutdown. Never call Stop from a request handler; Shutdown must
// arrange it asynchronously.
func (s *Service) Stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		if s.state == ServiceStateCreated {
			close(s.initDone) // no initialization task will run
		}
		s.state = ServiceStateStopped
		s.mu.Unlock()

		<-s.initDone
		conns := s.connections()
		for _, sc := range conns {
			sc.Close()
		}
		for _, sc := range conns {
			sc.WaitHandlers()
		}
	})
	return nil
}

// WaitUntilReady returns a channel closed when initialization finishes or the
// service is stopped before Start. Use IsActive to check whether it is ready.
func (s *Service) WaitUntilReady() <-chan struct{} { return s.initDone }

// IsActive reports whether initialization succeeded and the service is running.
func (s *Service) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == ServiceStateActive
}

// -- Private initialization and connection helpers ----------------------------

func (s *Service) initCluster() error {
	if err := s.connectSlaves(); err != nil {
		return err
	}
	// All slaves must see their master before any xshard dial. Each topology
	// response is awaited before the next slave is asked to dial, as in Python.
	for _, sc := range s.conns {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.StartupTimeout)
		err := sc.SendConnectToSlaves(ctx, s.cfg.SlaveInfos)
		cancel()
		if err != nil {
			return fmt.Errorf("interconnect slave %s: %w", sc.RemoteChainID(), err)
		}
	}
	root, err := s.backend.RootTip()
	if err != nil {
		return fmt.Errorf("get root checkpoint: %w", err)
	}
	if root == nil {
		return errors.New("missing root checkpoint")
	}
	errs := make([]error, len(s.cfg.SlaveInfos))
	var wg sync.WaitGroup
	for i, info := range s.cfg.SlaveInfos {
		sc := s.conns[info.ChainID.String()]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.pingSlave(sc, root); err != nil {
				errs[i] = fmt.Errorf("initialize slave %s: %w", sc.RemoteChainID(), err)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != ServiceStateStarting {
		return errors.New("master service stopped during initialization")
	}
	s.state = ServiceStateActive
	return nil
}

func (s *Service) connectSlaves() error {
	errs := make([]error, len(s.cfg.SlaveInfos))
	var wg sync.WaitGroup
	for i, info := range s.cfg.SlaveInfos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.connectSlave(info)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// pingSlave bounds both identity verification and shard initialization RPCs.
func (s *Service) pingSlave(sc *SlaveConn, root *qkctypes.RootBlock) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.StartupTimeout)
	defer cancel()
	return sc.SendPing(ctx, root)
}

func (s *Service) connectSlave(info clusternet.SlaveInfo) error {
	addr := net.JoinHostPort(info.Host, strconv.Itoa(int(info.Port)))
	var nc net.Conn
	for {
		s.mu.Lock()
		stopped := s.state == ServiceStateStopped
		s.mu.Unlock()
		if stopped {
			return context.Canceled
		}
		var err error
		nc, err = net.DialTimeout("tcp", addr, s.cfg.StartupTimeout)
		if err == nil {
			break
		}
		s.cfg.Logger.Warn("Failed to connect to slave, retrying", "chain", info.ChainID, "addr", addr, "err", err)
		time.Sleep(slaveConnectRetryDelay)
	}
	sc, err := NewSlaveConn(SlaveConnConfig{Conn: nc, RemoteChainID: info.ChainID, MaxPayloadSize: s.cfg.MaxPayloadSize, Handler: s, Logger: s.cfg.Logger})
	if err != nil {
		nc.Close()
		return err
	}
	s.mu.Lock()
	s.conns[info.ChainID.String()] = sc
	s.mu.Unlock()
	go func() {
		<-sc.WaitUntilClosed()
		s.shutdown()
	}()
	sc.Start()
	if err := s.pingSlave(sc, nil); err != nil {
		return fmt.Errorf("identify slave %s: %w", info.ChainID, err)
	}
	return nil
}

func (s *Service) connections() []*SlaveConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*SlaveConn, 0, len(s.conns))
	for _, sc := range s.conns {
		result = append(result, sc)
	}
	return result
}

// connection looks up a slave by chain ID without exposing connections to callers.
func (s *Service) connection(chainID *big.Int) *SlaveConn {
	if chainID == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != ServiceStateActive {
		return nil
	}
	return s.conns[chainID.String()]
}

// shutdown reports passive failure once. Stop marks the service as stopped
// before closing connections, so active shutdown is not reported.
func (s *Service) shutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		stopped := s.state == ServiceStateStopped
		s.mu.Unlock()
		if stopped {
			return
		}
		s.cfg.Shutdown()
	})
}

// -- Business calls into communication (master.ClusterClient) -----------------

// broadcastToSlaves sends to every slave concurrently and joins all failures.
func (s *Service) broadcastToSlaves(send func(*SlaveConn) error) error {
	s.mu.Lock()
	active := s.state == ServiceStateActive
	s.mu.Unlock()
	if !active {
		return errors.New("master service is not ready")
	}
	conns := s.connections()
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, sc := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := send(sc); err != nil {
				errs[i] = fmt.Errorf("slave %s: %w", sc.RemoteChainID(), err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// BroadcastRootBlock implements master.ClusterClient. Every configured slave
// must acknowledge the root; partial delivery is returned as an error for retry.
func (s *Service) BroadcastRootBlock(ctx context.Context, block *qkctypes.RootBlock) error {
	if block == nil {
		return errors.New("missing root block")
	}
	return s.broadcastToSlaves(func(sc *SlaveConn) error { return sc.SendAddRootBlock(ctx, block) })
}

// GetUnconfirmedHeaderList collects all chains, failing rather than omitting a
// missing slave. Filtering headers not yet reported to master is backend work.
func (s *Service) GetUnconfirmedHeaderList(ctx context.Context) (map[string][]*types.Header, error) {
	headers := make(map[string][]*types.Header)
	var mu sync.Mutex
	err := s.broadcastToSlaves(func(sc *SlaveConn) error {
		list, err := sc.SendGetUnconfirmedHeaders(ctx)
		if err != nil {
			return err
		}
		mu.Lock()
		headers[sc.RemoteChainID().String()] = list
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// GetRootChainStakes routes a query to the configured chain.
func (s *Service) GetRootChainStakes(ctx context.Context, chainID *big.Int, address common.Address, hash common.Hash) (*clusternet.RootChainStakesResult, error) {
	sc := s.connection(chainID)
	if sc == nil {
		return nil, fmt.Errorf("no slave connection for chain %v", chainID)
	}
	return sc.SendGetRootChainStakes(ctx, address, hash)
}

// -- Request handlers calling business methods -------------------------------

// AddShardBlockHeader forwards the slave report to the backend.
func (s *Service) AddShardBlockHeader(chainID *big.Int, header *types.Header, amount *uint256.Int) error {
	return s.backend.AddShardBlockHeader(chainID, header, amount)
}
