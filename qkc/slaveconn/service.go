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
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/qkc/wire"
	"github.com/holiman/uint256"
)

// ShardBackend is the business state borrowed by Service. Its methods must be
// safe for concurrent calls. Service never creates or closes the backend.
// Return *comm.RemoteError for an explicit protocol error code; other errors
// close the connection. The existing shard.Shard satisfies this interface implicitly.
type ShardBackend interface {
	Init(*qkctypes.RootBlock) error
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
	// Shutdown requests the owning node's shutdown. It is required and must be
	// concurrency-safe, idempotent and return promptly: it must only start an
	// asynchronous node shutdown, never synchronously call Stop or Node.Close.
	// Multiple connection failures and normal Close callbacks may invoke it.
	Shutdown func()
}

// Service owns the slave's listener and cluster connections. The owner creates
// it first, passes it to Shard as ClusterClient, binds the Shard, then starts it.
// Only the owner manages execution resources and node shutdown.
// The first accepted connection must be Master; Master must connect to every
// slave before instructing any slave to dial another, as required by the manager.
type Service struct {
	cfg           ServiceConfig
	backend       ShardBackend
	xshardManager *XshardConnManager
	master        *MasterConn
	listener      net.Listener

	mu       sync.Mutex // protects lifecycle
	stopOnce sync.Once
	stopping bool
}

var (
	_ ShardBackend         = (*shard.Shard)(nil)
	_ shard.ClusterClient  = (*Service)(nil)
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

// Stop closes the listener and connections.
// It does not cancel or wait for running backend calls, or close the backend.
func (s *Service) Stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		listener, master := s.listener, s.master
		s.mu.Unlock()

		if listener != nil {
			listener.Close()
		}
		s.xshardManager.Close()
		if master != nil {
			master.Close()
		}
	})
	return nil
}

// shutdown immediately reports connection/listener failure to the owner.
// The owner schedules node shutdown asynchronously. Service.Stop performs
// the communication resource cleanup.
func (s *Service) shutdown() {
	s.cfg.Shutdown()
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
func (s *Service) DialToSlave(ctx context.Context, info wire.SlaveInfo) error {
	return s.xshardManager.DialToSlave(ctx, info)
}

// -- Business calls into communication (shard.ClusterClient) ------------------

// BroadcastXShardTxList implements shard.ClusterClient for the current topology
// of one host per chain. Empty deposit lists are sent and require an ACK too.
func (s *Service) BroadcastXShardTxList(ctx context.Context, chainID *big.Int, hash common.Hash, deposits []*types.DepositTx) error {
	c := s.xshardManager.Lookup(chainID)
	if c == nil {
		return fmt.Errorf("no xshard connection for chain %v", chainID)
	}
	return c.SendAddXshardTxList(ctx, hash, deposits)
}

// SendShardBlockHeaderToMaster implements shard.ClusterClient. Success means
// that Master returned a zero ErrorCode, not merely that the write completed.
func (s *Service) SendShardBlockHeaderToMaster(ctx context.Context, header *types.Header, amount *uint256.Int) error {
	s.mu.Lock()
	master := s.master
	s.mu.Unlock()
	if master == nil {
		return errors.New("master connection is not established")
	}
	return master.SendAddShardBlockHeader(ctx, header, amount)
}

// -- Request handlers calling business methods -------------------------------

// CreateShards handles Master's PING(rootTip) after slave interconnection.
// The single Shard already exists; Init establishes its root checkpoint.
func (s *Service) CreateShards(root *qkctypes.RootBlock) error {
	return s.backend.Init(root)
}

func (s *Service) AddRootBlock(root *qkctypes.RootBlock) error {
	_, err := s.backend.AddRootBlock(root)
	return err
}

func (s *Service) GetRootChainStakes(address common.Address, hash common.Hash) (*comm.RootChainStakesResult, error) {
	stakes, signer, err := s.backend.GetRootChainStakes(address, hash)
	if err != nil {
		return nil, err
	}
	return &comm.RootChainStakesResult{Stakes: stakes, Signer: signer}, nil
}

func (s *Service) GetUnconfirmedHeaders() ([]*types.Header, error) {
	headers, err := s.backend.GetUnconfirmedHeaderList()
	return headers, err
}

func (s *Service) AddXshardTxList(hash common.Hash, deposits []*types.DepositTx) error {
	return s.backend.AddXshardTxList(hash, deposits)
}
