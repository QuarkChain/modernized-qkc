// Copyright 2026 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

// Temporary milestone 1 test harness, enabled only by --m1. It uses an offline
// R0 with real master/slave connections, shard backend, consensus and importer.
// Cross-shard delivery remains mocked.
// TODO(m1): Remove this file and its startup hooks when master-driven shard
// lifecycle and mining control are available.

import (
	"context"
	"errors"
	"math/big"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/ethereum/go-ethereum/qkc/slaveconn"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
	"github.com/urfave/cli/v2"
)

var m1Flag = &cli.BoolFlag{
	Name:  "m1",
	Usage: "Enable milestone 1 cluster connections, automatic simulated mining and mock cross-shard delivery; disable P2P",
}

func configureM1(ctx *cli.Context, cfg *gethConfig) error {
	if !ctx.Bool(m1Flag.Name) {
		return nil
	}
	if cfg.Node.PrivateHost == "" || cfg.Node.PrivatePort <= 0 || cfg.Node.PrivatePort > 65535 {
		return errors.New("--m1 requires Node.PrivateHost and a valid Node.PrivatePort")
	}
	// Both shards can run locally without competing for the default P2P port.
	cfg.Node.P2P.ListenAddr, cfg.Node.P2P.DiscAddr = "", ""
	cfg.Node.P2P.MaxPeers = 0
	cfg.Node.P2P.NoDial, cfg.Node.P2P.NoDiscovery = true, true
	cfg.Node.P2P.DiscoveryV4, cfg.Node.P2P.DiscoveryV5 = false, false
	return nil
}

type m1Service struct {
	mu       sync.Mutex
	stack    *node.Node
	backend  *shard.Shard
	miner    *shard.PoWMiner
	chainID  *big.Int
	listener net.Listener
	master   *slaveconn.MasterConn
	running  bool
}

// Supply the demo's communication and search dependencies before eth.New.
// Ethereum constructs and owns the single Shard/PoWMiner pair.
func newM1Service(stack *node.Node, cfg *ethconfig.Config) *m1Service {
	service := &m1Service{stack: stack}
	cfg.QKCClusterClient = service
	// The one-second mean search delay is a demo setting.
	cfg.QKCMiningSearch = shard.NewSimulatedSearch(time.Second)
	return service
}

func (s *m1Service) register(backend *eth.Ethereum) error {
	config := backend.BlockChain().Config()
	if config.QuarkChain == nil {
		return errors.New("--m1 requires a QuarkChain shard")
	}
	s.backend = backend.Shard()
	s.chainID = config.ChainID
	if config.QuarkChain.ConsensusType == params.ConsensusSimulate {
		s.miner = backend.PoWMiner()
	}
	// Reverse lifecycle order closes M1's connections before Ethereum stops
	// its miner and shared chain resources.
	s.stack.RegisterLifecycle(s)
	log.Warn("Milestone 1 enabled: cross-shard delivery is mocked")
	return nil
}

func (s *m1Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.stack.Config()
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.PrivateHost, strconv.Itoa(cfg.PrivatePort)))
	if err != nil {
		return err
	}
	s.listener = listener
	s.running = true
	go s.acceptMaster(listener)
	log.Info("Milestone 1 waiting for master", "endpoint", listener.Addr())
	return nil
}

// The demo accepts one master for its lifetime. A disconnected master shuts down
// geth through the connection's normal ownership callback; reconnect is not tested.
func (s *m1Service) acceptMaster(listener net.Listener) {
	defer listener.Close()
	socket, err := listener.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		socket.Close()
		return
	}
	master, err := slaveconn.NewMasterConn(slaveconn.MasterConnConfig{
		Conn: socket, LocalChainID: s.chainID, Handler: s, SlaveDialer: s,
		ShutdownSlave: func() { s.stack.Close() },
	})
	if err != nil {
		socket.Close()
		log.Error("Milestone 1 master connection failed", "err", err)
		go s.stack.Close()
		return
	}
	s.master = master
	master.Start()
}

func (s *m1Service) Stop() error {
	s.mu.Lock()
	s.running = false
	if s.listener != nil {
		s.listener.Close()
	}
	if s.master != nil {
		// Wake any pending header report before waiting for mining to stop.
		s.master.Close()
	}
	s.mu.Unlock()
	return nil
}

// CreateShards handles PING(R0). M1 reuses geth's existing shard and starts its
// local miner instead of implementing the full master-driven shard lifecycle.
func (s *m1Service) CreateShards(root *qkctypes.RootBlock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return errors.New("milestone 1 service is not running")
	}
	if root.Number() != 0 || root.ParentHash() != (common.Hash{}) {
		return errors.New("milestone 1 requires root genesis")
	}
	if root.ShardHeaderHash() != qkctypes.CalcShardHeaderHash(root.ShardBlockHeaders()) {
		return errors.New("root genesis body commitment mismatch")
	}
	hash := root.Hash()
	if tip := s.backend.RootTip(); tip != (common.Hash{}) {
		if tip != hash {
			return errors.New("shard is already initialized with a different root")
		}
		return nil
	}
	// Init selects the confirmed G0. Do not rewind blocks from an earlier demo.
	if s.backend.CurrentBlock().Number.Sign() != 0 {
		return errors.New("milestone 1 requires a genesis-only data directory; use a fresh --datadir")
	}
	if err := s.backend.Init(root); err != nil {
		return err
	}
	if s.miner != nil {
		if err := s.miner.StartLocalMining(); err != nil {
			return err
		}
	}
	log.Info("Initialized milestone 1 root", "hash", hash, "mining", s.miner != nil)
	return nil
}

// Cross-shard delivery is still a test stub. Header reporting below is real.
func (s *m1Service) BroadcastXShardTxList(ctx context.Context, _ *big.Int, _ common.Hash, _ []*types.DepositTx) error {
	return ctx.Err()
}

func (s *m1Service) SendShardBlockHeaderToMaster(ctx context.Context, header *types.Header, amount *uint256.Int) error {
	s.mu.Lock()
	master, running := s.master, s.running
	s.mu.Unlock()
	if !running || master == nil {
		return errors.New("milestone 1 has no master connection")
	}
	return master.SendAddShardBlockHeader(ctx, header, amount)
}

// The remaining cluster operations are outside this temporary demo's scope.
func (s *m1Service) DialToSlave(context.Context, comm.SlaveInfo) error {
	return errors.New("milestone 1 does not connect shards to each other")
}

func (s *m1Service) AddRootBlock(*qkctypes.RootBlock) error {
	return errors.New("milestone 1 only supports initialization with R0")
}

func (s *m1Service) GetRootChainStakes(common.Address, common.Hash) (*comm.RootChainStakesResult, error) {
	return nil, errors.New("milestone 1 does not query root-chain stakes")
}

func (s *m1Service) GetUnconfirmedHeaders() ([]*types.Header, error) {
	return nil, errors.New("milestone 1 uses pushed shard headers")
}
