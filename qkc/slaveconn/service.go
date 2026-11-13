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
	"fmt"
	"math/big"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/comm"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

// ShardBackend supplies concurrent-safe business operations borrowed by Service.
// Init errors close the master connection. Other methods may return
// *comm.RemoteError as a protocol rejection; all other errors close the connection.
type ShardBackend interface {
	// The owner observes successful initialization through the backend before
	// starting local business activity.
	Init(*qkctypes.RootBlock) error
	// The result reports whether the root became the tip; Service ignores it.
	AddRootBlock(*qkctypes.RootBlock) (bool, error)
	GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error)
	GetUnconfirmedHeaderList() ([]*types.Header, error)
	AddXshardTxList(common.Hash, []*types.DepositTx) error
}

// ServiceConfig supplies validated deployment settings, not wire messages.
// ConfiguredChainIDs includes the local chain and allowed remote chains.
type ServiceConfig struct {
	ListenAddr         string
	LocalChainID       *big.Int
	ConfiguredChainIDs []*big.Int
	MaxPayloadSize     uint32
	Logger             log.Logger
	// OnInitialized lets the owner enable business activity after Init succeeds.
	// It runs as part of the initialization request and must not stop the service.
	OnInitialized func() error
	// Shutdown requests asynchronous node shutdown on disconnect or listener failure.
	// It must be prompt, concurrent-safe and idempotent. The owner closes Service;
	// Master's owner stops the cluster.
	Shutdown func()
}

// Service owns the slave listener and cluster connections, not shard resources.
// Assembly is NewService -> shard.New(client=Service) -> BindShard -> Start.
// Master must connect to every slave first (the first connection is treated as
// Master), then await each slave's interconnection command in turn, aborting on
// failure. PING(rootTip) initializes the shard afterward; Service trusts this order.
// Outbound RPC callers supply context deadlines and handle errors. Only connection
// closure or listener failure requests shutdown; RPC rejection/timeout alone does not.
type Service struct {
	cfg           ServiceConfig
	backend       ShardBackend
	xshardManager *XshardConnManager
	master        *MasterConn
	listener      net.Listener

	mu       sync.Mutex // protects lifecycle and request admission
	stopOnce sync.Once
	stopping bool
	calls    sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

var (
	_ MasterRequestHandler = (*Service)(nil)
	_ XshardRequestHandler = (*Service)(nil)
	_ SlaveDialer          = (*Service)(nil)
)

// -- Communication lifecycle and topology ------------------------------------

// NewService creates an inactive service without opening a listener or invoking
// the backend. LocalChainID is copied; the manager records configured IDs.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("missing slave listen address")
	}
	if cfg.Shutdown == nil {
		return nil, errors.New("missing node shutdown callback")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Root()
	}
	s := &Service{cfg: cfg}
	manager, err := NewXshardConnManager(cfg.LocalChainID, cfg.ConfiguredChainIDs, cfg.MaxPayloadSize, s, s.shutdown, cfg.Logger)
	if err != nil {
		return nil, err
	}
	s.xshardManager = manager
	s.cfg.LocalChainID = new(big.Int).Set(cfg.LocalChainID)
	s.cfg.ConfiguredChainIDs = make([]*big.Int, len(cfg.ConfiguredChainIDs))
	for i, id := range cfg.ConfiguredChainIDs {
		s.cfg.ConfiguredChainIDs[i] = new(big.Int).Set(id)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// BindShard completes assembly exactly once, before Start. The owner calls it
// during sequential construction, never concurrently with other service methods.
func (s *Service) BindShard(backend ShardBackend) error {
	if backend == nil {
		return errors.New("missing shard backend")
	}
	if s.backend != nil || s.listener != nil || s.stopping {
		return errors.New("shard backend must be bound once before start")
	}
	s.backend = backend
	return nil
}

// Start listens without waiting for Master or root initialization. The owner
// calls it once after BindShard and before Stop, without concurrent lifecycle calls.
func (s *Service) Start() error {
	if s.listener != nil || s.stopping {
		return errors.New("slave service already started or stopped")
	}
	if s.backend == nil {
		return errors.New("missing shard backend")
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.listener = listener
	go s.acceptLoop(listener)
	s.cfg.Logger.Info("Slave waiting for master", "endpoint", listener.Addr())
	return nil
}

// Addr returns the bound address, or nil before Start. It must not be called
// concurrently with Start.
func (s *Service) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Quiesce rejects new calls and cancels outbound work before the owner stops
// mining. Synchronous backend calls finish normally and are joined by Stop.
func (s *Service) Quiesce() {
	s.mu.Lock()
	s.stopping = true
	listener := s.listener
	s.mu.Unlock()
	s.cancel()
	if listener != nil {
		listener.Close()
	}
}

// Stop drains admitted calls, then closes connections. The owner must quiesce
// and join its own tasks before releasing shared shard resources. Never call
// Stop from a request handler: it would wait for that same handler to return.
func (s *Service) Stop() error {
	s.stopOnce.Do(func() {
		s.Quiesce()
		s.calls.Wait()
		s.xshardManager.Close()
		s.mu.Lock()
		master := s.master
		s.mu.Unlock()
		if master != nil {
			master.Close()
		}
	})
	return nil
}

func (s *Service) shutdown() {
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	if !stopping {
		s.cfg.Shutdown()
	}
}

// beginCall pairs admission with Stop's wait under the same lifecycle lock.
func (s *Service) beginCall() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.listener == nil {
		return errors.New("slave service is not running")
	}
	s.calls.Add(1)
	return nil
}

func (s *Service) callContext(ctx context.Context) (context.Context, func(), error) {
	if err := s.beginCall(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel(); s.calls.Done() }, nil
}

// ConnectionsReady reports whether Master and every configured remote chain
// are connected. A subsequent disconnect requests node shutdown.
func (s *Service) ConnectionsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.master == nil || !s.master.IsActive() {
		return false
	}
	for _, id := range s.cfg.ConfiguredChainIDs {
		if id.Cmp(s.cfg.LocalChainID) == 0 {
			continue
		}
		if c := s.xshardManager.Lookup(id); c == nil || !c.IsActive() {
			return false
		}
	}
	return true
}

func (s *Service) acceptLoop(listener net.Listener) {
	for {
		nc, err := listener.Accept()
		if err != nil {
			s.shutdown()
			return
		}

		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			nc.Close()
			return
		}
		if s.master == nil {
			mc, err := NewMasterConn(MasterConnConfig{
				Conn: nc, LocalChainID: s.cfg.LocalChainID,
				MaxPayloadSize: s.cfg.MaxPayloadSize, Handler: s,
				SlaveDialer: s, ShutdownSlave: s.shutdown, Logger: s.cfg.Logger,
			})
			if err != nil {
				s.mu.Unlock()
				nc.Close()
				s.shutdown()
				return
			}
			s.master = mc
			mc.Start()
		} else {
			go s.xshardManager.HandleInbound(nc)
		}
		s.mu.Unlock()
	}
}

// DialToSlave connects a slave advertised by Master through the connection manager.
// It reuses known connections and does not retry or reconnect after failure.
// Command failures are returned to Master, whose owner must abort cluster startup.
func (s *Service) DialToSlave(ctx context.Context, info comm.SlaveInfo) error {
	ctx, done, err := s.callContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	return s.xshardManager.DialToSlave(ctx, info)
}

// -- Business calls into communication (shard.ClusterClient) ------------------

// BroadcastXShardTxList implements shard.ClusterClient for the current topology
// of one host per chain. Empty deposit lists are sent and require an ACK too.
func (s *Service) BroadcastXShardTxList(ctx context.Context, chainID *big.Int, hash common.Hash, deposits []*types.DepositTx) error {
	ctx, done, err := s.callContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	c := s.xshardManager.Lookup(chainID)
	if c == nil {
		return fmt.Errorf("no xshard connection for chain %v", chainID)
	}
	// BaseConn cancellation does not interrupt writes. Closing an active RPC's
	// socket on service shutdown unblocks it before Stop joins the caller.
	stop := context.AfterFunc(s.ctx, c.Close)
	defer stop()
	return c.SendAddXshardTxList(ctx, hash, deposits)
}

// SendShardBlockHeaderToMaster implements shard.ClusterClient. Success means
// that Master returned a zero ErrorCode, not merely that the write completed.
func (s *Service) SendShardBlockHeaderToMaster(ctx context.Context, header *types.Header, amount *uint256.Int) error {
	ctx, done, err := s.callContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	s.mu.Lock()
	master := s.master
	s.mu.Unlock()
	if master == nil {
		return errors.New("master connection is not established")
	}
	stop := context.AfterFunc(s.ctx, master.Close)
	defer stop()
	return master.SendAddShardBlockHeader(ctx, header, amount)
}

// -- Request handlers calling business methods -------------------------------

// CreateShards handles Master's PING(rootTip) after slave interconnection.
// The single Shard already exists; Init establishes its root checkpoint.
func (s *Service) CreateShards(root *qkctypes.RootBlock) error {
	if err := s.beginCall(); err != nil {
		return err
	}
	defer s.calls.Done()
	if err := s.backend.Init(root); err != nil {
		return err
	}
	if s.cfg.OnInitialized != nil {
		return s.cfg.OnInitialized()
	}
	return nil
}

func (s *Service) AddRootBlock(root *qkctypes.RootBlock) error {
	if err := s.beginCall(); err != nil {
		return err
	}
	defer s.calls.Done()
	_, err := s.backend.AddRootBlock(root)
	return err
}

func (s *Service) GetRootChainStakes(address common.Address, hash common.Hash) (*comm.RootChainStakesResult, error) {
	if err := s.beginCall(); err != nil {
		return nil, err
	}
	defer s.calls.Done()
	stakes, signer, err := s.backend.GetRootChainStakes(address, hash)
	if err != nil {
		return nil, err
	}
	return &comm.RootChainStakesResult{Stakes: stakes, Signer: signer}, nil
}

func (s *Service) GetUnconfirmedHeaders() ([]*types.Header, error) {
	if err := s.beginCall(); err != nil {
		return nil, err
	}
	defer s.calls.Done()
	headers, err := s.backend.GetUnconfirmedHeaderList()
	return headers, err
}

func (s *Service) AddXshardTxList(hash common.Hash, deposits []*types.DepositTx) error {
	if err := s.beginCall(); err != nil {
		return err
	}
	defer s.calls.Done()
	return s.backend.AddXshardTxList(hash, deposits)
}
