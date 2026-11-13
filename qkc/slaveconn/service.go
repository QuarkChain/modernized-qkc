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
	"syscall"

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
	// Shutdown requests node shutdown on unexpected disconnect or listener failure.
	// It is called at most once, never as a result of Stop. It must return promptly and
	// schedule shutdown asynchronously. The owner stops Service before releasing
	// shard resources; Master's owner stops the cluster.
	Shutdown func()
}

// Service owns the slave listener and cluster connections, not shard resources.
// Assembly is NewService -> shard.New(client=Service) -> Start(shard).
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

	mu           sync.Mutex // protects lifecycle
	stopOnce     sync.Once
	shutdownOnce sync.Once
	stopping     bool
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
	return s, nil
}

// Start installs the backend and listens without waiting for Master or root
// initialization.
func (s *Service) Start(backend ShardBackend) error {
	if s.listener != nil || s.stopping {
		return errors.New("slave service already started or stopped")
	}
	if backend == nil {
		return errors.New("missing shard backend")
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.backend = backend
	s.listener = listener
	go s.acceptLoop(listener)
	return nil
}

// Addr returns the bound address, or nil before Start.
func (s *Service) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Stop closes the listener and connections, then waits for admitted request
// handlers to finish. It does not cancel handlers; topology handlers may finish
// pending dials before returning. Late dial results are closed by the manager.
// The owner may release shard resources after Stop returns. Normal Stop does not
// request Shutdown. Stop must not be called from a request handler.
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
			master.WaitHandlers()
		}
		s.xshardManager.WaitHandlers()
	})
	return nil
}

// shutdown forwards the first unexpected failure to the owner.
// Stop sets stopping before closing anything, so its failures are ignored.
func (s *Service) shutdown() {
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

func (s *Service) acceptLoop(listener net.Listener) {
	for {
		nc, err := listener.Accept()
		// An aborted pending connection does not invalidate the listener.
		if errors.Is(err, syscall.ECONNABORTED) {
			continue
		}
		if err != nil {
			s.cfg.Logger.Debug("Accept failed", "err", err)
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
				SlaveDialer: s, Logger: s.cfg.Logger,
			})
			if err != nil {
				s.mu.Unlock()
				nc.Close()
				s.shutdown()
				return
			}
			s.master = mc
			go func() {
				<-mc.WaitUntilClosed()
				s.shutdown()
			}()
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
