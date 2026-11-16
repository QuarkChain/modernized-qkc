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
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/conn"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
	"golang.org/x/sync/errgroup"
)

// MasterBackend supplies concurrent-safe business operations borrowed by Service.
// Business rejections use *clusternet.RemoteError; other handler errors close the connection.
type MasterBackend interface {
	// RootTip returns the recovered checkpoint used to initialize all shards.
	// It must return promptly; startup cancellation does not interrupt backend calls.
	RootTip() (*qkctypes.RootBlock, error)
	AddShardBlockHeader(sourceChainID *big.Int, header *types.Header, coinbaseAmount *uint256.Int) error
}

// ServiceConfig supplies deployment settings for one slave per configured chain.
type ServiceConfig struct {
	Slaves             []clusternet.SlaveInfo
	ConfiguredChainIDs []*big.Int
	MaxPayloadSize     uint32
	// StartupTimeout bounds cluster initialization communication. Zero uses one minute.
	StartupTimeout time.Duration
	Logger         log.Logger
	// Shutdown requests asynchronous node shutdown after an unexpected slave
	// disconnect. It is called at most once, never as a result of Stop or failed
	// startup (reported by Start). It must return promptly. The owner stops Service
	// before releasing master resources and stops the rest of the cluster.
	Shutdown func()
}

// Service owns master-to-slave connections, not root-chain or mining resources.
// Assembly is NewService -> master.New(client=Service) -> Start(master).
// Start completes identity checks, sequential slave interconnection, and shard
// initialization before returning. The owner then recovers unfinished root
// broadcasts before starting normal business activity. Startup is attempted once;
// failed startup closes and drains every connection, without retry or reconnect.
type Service struct {
	cfg     ServiceConfig
	backend MasterBackend

	mu           sync.Mutex            // protects lifecycle and connection ownership
	conns        map[string]*SlaveConn // includes connections still verifying identity
	started      bool
	ready        bool
	stopping     bool
	startCancel  context.CancelFunc
	stopOnce     sync.Once
	shutdownOnce sync.Once
}

var _ SlaveRequestHandler = (*Service)(nil)

// -- Communication lifecycle and topology ------------------------------------

// NewService validates configuration without opening connections.
// The caller must not modify configuration slices or chain IDs afterward.
func NewService(cfg ServiceConfig) (*Service, error) {
	if len(cfg.ConfiguredChainIDs) == 0 {
		return nil, errors.New("configured chain IDs must not be empty")
	}
	if cfg.Shutdown == nil {
		return nil, errors.New("missing node shutdown callback")
	}
	if len(cfg.ConfiguredChainIDs) != len(cfg.Slaves) {
		return nil, errors.New("configured chain IDs and slaves must have equal counts")
	}
	payloadSize, err := conn.ClusterPayloadSize(cfg.MaxPayloadSize)
	if err != nil {
		return nil, err
	}
	cfg.MaxPayloadSize = payloadSize
	if cfg.StartupTimeout < 0 {
		return nil, errors.New("negative cluster startup timeout")
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Root()
	}
	configured := make(map[string]struct{}, len(cfg.ConfiguredChainIDs))
	for _, id := range cfg.ConfiguredChainIDs {
		if err := conn.ValidateSlaveChainID(id); err != nil {
			return nil, err
		}
		configured[id.String()] = struct{}{}
	}
	for i, info := range cfg.Slaves {
		if info.Host == "" || info.Port == 0 {
			return nil, fmt.Errorf("invalid slave address at index %d", i)
		}
		if err := conn.ValidateSlaveChainID(info.ChainID); err != nil {
			return nil, err
		}
		key := info.ChainID.String()
		if _, ok := configured[key]; !ok {
			return nil, fmt.Errorf("slave chain ID %s is unconfigured or duplicated", key)
		}
		delete(configured, key)
	}
	return &Service{cfg: cfg, conns: make(map[string]*SlaveConn)}, nil
}

// Start installs the backend and synchronously initializes the whole cluster.
// The owner calls Start and Stop serially. Startup failure closes connections
// and drains handlers before returning; restart requires a new service.
func (s *Service) Start(backend MasterBackend) (err error) {
	if s.started || s.stopping {
		return errors.New("master service already started or stopped")
	}
	if backend == nil {
		return errors.New("missing master backend")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.StartupTimeout)
	s.backend, s.started, s.startCancel = backend, true, cancel
	defer func() {
		cancel()
		if err != nil {
			s.Stop()
		}
	}()
	root, err := backend.RootTip()
	if err != nil {
		return fmt.Errorf("get root checkpoint: %w", err)
	}
	if root == nil {
		return errors.New("missing root checkpoint")
	}
	if err := s.connectSlaves(ctx); err != nil {
		return err
	}
	// All slaves must see their master before any xshard dial. Each topology
	// response is awaited before the next slave is asked to dial, as in Python.
	for _, info := range s.cfg.Slaves {
		if err := s.connection(info.ChainID).SendConnectToSlaves(ctx, s.cfg.Slaves); err != nil {
			return fmt.Errorf("interconnect slave %s: %w", info.ChainID, err)
		}
	}
	group, initCtx := errgroup.WithContext(ctx)
	for _, info := range s.cfg.Slaves {
		group.Go(func() error {
			if err := s.connection(info.ChainID).SendPing(initCtx, root); err != nil {
				return fmt.Errorf("initialize slave %s: %w", info.ChainID, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, sc := range s.conns {
		if sc.IsClosed() {
			return fmt.Errorf("slave %s disconnected during startup", sc.RemoteChainID())
		}
	}
	s.ready = true
	return nil
}

func (s *Service) connectSlaves(ctx context.Context) error {
	group, dialCtx := errgroup.WithContext(ctx)
	for _, info := range s.cfg.Slaves {
		group.Go(func() error {
			addr := net.JoinHostPort(info.Host, strconv.Itoa(int(info.Port)))
			nc, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
			if err != nil {
				return fmt.Errorf("dial slave %s at %s: %w", info.ChainID, addr, err)
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
			if err := sc.SendPing(dialCtx, nil); err != nil {
				return fmt.Errorf("identify slave %s: %w", info.ChainID, err)
			}
			return nil
		})
	}
	return group.Wait()
}

// Stop closes every connection before waiting and drains all
// admitted handlers. It does not cancel backend calls or stop backend resources.
// The owner may release master resources after Stop returns. Normal Stop does
// not request Shutdown. Never call Stop from a request handler; Shutdown must
// arrange it asynchronously.
func (s *Service) Stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping, s.ready = true, false
		s.mu.Unlock()
		for _, sc := range s.connections() {
			sc.Close()
		}
		for _, sc := range s.connections() {
			sc.WaitHandlers()
		}
	})
	return nil
}

// shutdown forwards the first unexpected disconnect to the owner. During
// startup it cancels initialization instead, leaving Start to report the failure.
// Stop marks the service as stopping before closing connections, so those
// disconnects never request node shutdown. The owner performs cleanup via Stop.
func (s *Service) shutdown() {
	s.mu.Lock()
	stopping, ready, cancel := s.stopping, s.ready, s.startCancel
	s.mu.Unlock()
	if stopping {
		return
	}
	if !ready {
		if cancel != nil {
			cancel()
		}
		return
	}
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}
		s.cfg.Shutdown()
	})
}

func (s *Service) connection(chainID *big.Int) *SlaveConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[chainID.String()]
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

func (s *Service) readyConnections() ([]*SlaveConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.stopping {
		return nil, errors.New("master service is not ready")
	}
	result := make([]*SlaveConn, 0, len(s.cfg.Slaves))
	for _, info := range s.cfg.Slaves {
		sc := s.conns[info.ChainID.String()]
		if sc == nil {
			return nil, fmt.Errorf("no slave connection for chain %s", info.ChainID)
		}
		result = append(result, sc)
	}
	return result, nil
}

// -- Business calls into communication (master.ClusterClient) -----------------

// BroadcastRootBlock implements master.ClusterClient. Every configured slave
// must acknowledge the root; partial delivery is returned as an error for retry.
func (s *Service) BroadcastRootBlock(ctx context.Context, block *qkctypes.RootBlock) error {
	if block == nil {
		return errors.New("missing root block")
	}
	return s.forEachSlave(ctx, func(ctx context.Context, sc *SlaveConn) error { return sc.SendAddRootBlock(ctx, block) })
}

// GetUnconfirmedHeaderList collects all chains, failing rather than omitting a
// missing slave. Filtering headers not yet reported to master is backend work.
func (s *Service) GetUnconfirmedHeaderList(ctx context.Context) (map[string][]*types.Header, error) {
	headers := make(map[string][]*types.Header)
	var mu sync.Mutex
	err := s.forEachSlave(ctx, func(ctx context.Context, sc *SlaveConn) error {
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

func (s *Service) forEachSlave(ctx context.Context, call func(context.Context, *SlaveConn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conns, err := s.readyConnections()
	if err != nil {
		return err
	}
	group, ctx := errgroup.WithContext(ctx)
	for _, sc := range conns {
		group.Go(func() error {
			if err := call(ctx, sc); err != nil {
				return fmt.Errorf("slave %s: %w", sc.RemoteChainID(), err)
			}
			return nil
		})
	}
	return group.Wait()
}

// GetRootChainStakes routes a query to the configured chain.
func (s *Service) GetRootChainStakes(ctx context.Context, chainID *big.Int, address common.Address, hash common.Hash) (*clusternet.RootChainStakesResult, error) {
	if chainID == nil {
		return nil, errors.New("missing chain ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conns, err := s.readyConnections()
	if err != nil {
		return nil, err
	}
	for _, sc := range conns {
		if sc.RemoteChainID().Cmp(chainID) == 0 {
			return sc.SendGetRootChainStakes(ctx, address, hash)
		}
	}
	return nil, fmt.Errorf("no slave connection for chain %s", chainID)
}

// -- Request handlers calling business methods -------------------------------

// AddShardBlockHeader forwards a slave's decoded report to the borrowed backend.
func (s *Service) AddShardBlockHeader(chainID *big.Int, header *types.Header, amount *uint256.Int) error {
	return s.backend.AddShardBlockHeader(chainID, header, amount)
}
