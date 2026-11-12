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

// Temporary milestone 1 test harness, enabled only by --ms1. It uses an offline
// R0 and mocked cluster delivery with the real shard backend, consensus engine
// and block importer.
// TODO(ms1): Remove this file and its startup hooks when master-driven shard
// initialization, mining control and cluster transport are available.

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"github.com/urfave/cli/v2"
)

var ms1Flag = &cli.BoolFlag{
	Name:  "ms1",
	Usage: "Enable milestone 1 test RPC, automatic simulated mining and mock cluster delivery; disable P2P",
}

func configureMS1(ctx *cli.Context, cfg *gethConfig) error {
	if !ctx.Bool(ms1Flag.Name) {
		return nil
	}
	if cfg.Node.IPCPath == "" {
		return errors.New("--ms1 requires IPC")
	}
	// Both shards can run locally without competing for the default P2P port.
	cfg.Node.P2P.ListenAddr, cfg.Node.P2P.DiscAddr = "", ""
	cfg.Node.P2P.MaxPeers = 0
	cfg.Node.P2P.NoDial, cfg.Node.P2P.NoDiscovery = true, true
	cfg.Node.P2P.DiscoveryV4, cfg.Node.P2P.DiscoveryV5 = false, false
	return nil
}

func registerMS1(stack *node.Node, backend *eth.Ethereum) error {
	service, err := newMS1Service(backend, stack.Config().CoinbaseAddress)
	if err != nil {
		return err
	}
	stack.RegisterAPIs([]rpc.API{{Namespace: "ms1", Service: &ms1API{service}}})
	// Ethereum was registered first, so mining stops before its chain is closed.
	stack.RegisterLifecycle(service)
	log.Warn("Milestone 1 enabled: cluster delivery is mocked")
	return nil
}

type ms1Service struct {
	mu      sync.Mutex
	backend *shard.Shard
	miner   *shard.PoWMiner
	running bool
}

func newMS1Service(backend *eth.Ethereum, coinbase common.Address) (*ms1Service, error) {
	config := backend.BlockChain().Config()
	if config.QuarkChain == nil {
		return nil, errors.New("--ms1 requires a QuarkChain shard")
	}
	chain := shard.NewShardChain(backend.ChainDb(), backend.BlockChain(), backend.Miner())
	s, err := shard.NewWithChain(chain, ms1ClusterClient{})
	if err != nil {
		return nil, err
	}
	service := &ms1Service{backend: s}
	if config.QuarkChain.ConsensusType == params.ConsensusSimulate {
		// The one-second mean search delay is a demo setting.
		service.miner = shard.NewPoWMiner(s, shard.PoWMinerConfig{
			Coinbase: coinbase,
			Search:   shard.NewSimulatedSearch(time.Second),
		})
	}
	return service, nil
}

func (s *ms1Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = true
	return nil
}

func (s *ms1Service) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	if s.miner != nil {
		s.miner.StopLocalMining()
	}
	return nil
}

// Keep lifecycle methods off the RPC receiver: ms1 exposes only initRoot.
type ms1API struct{ service *ms1Service }

// InitRoot is the temporary ms1_initRoot RPC. It substitutes for master's
// initialization and mining-start requests and accepts only root genesis.
func (api *ms1API) InitRoot(data hexutil.Bytes) (common.Hash, error) {
	s := api.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return common.Hash{}, errors.New("milestone 1 service is not running")
	}
	root := new(qkctypes.RootBlock)
	if err := root.UnmarshalSSZ(data); err != nil {
		return common.Hash{}, err
	}
	if root.Number() != 0 || root.ParentHash() != (common.Hash{}) {
		return common.Hash{}, errors.New("milestone 1 requires root genesis")
	}
	if root.ShardHeaderHash() != qkctypes.CalcShardHeaderHash(root.ShardBlockHeaders()) {
		return common.Hash{}, errors.New("root genesis body commitment mismatch")
	}
	hash := root.Hash()
	if tip := s.backend.RootTip(); tip != (common.Hash{}) {
		if tip != hash {
			return common.Hash{}, errors.New("shard is already initialized with a different root")
		}
		return hash, nil
	}
	// Init selects the confirmed G0. Do not rewind blocks from an earlier demo.
	if s.backend.CurrentBlock().Number.Sign() != 0 {
		return common.Hash{}, errors.New("milestone 1 requires a genesis-only data directory; use a fresh --datadir")
	}
	if err := s.backend.Init(root); err != nil {
		return common.Hash{}, err
	}
	if s.miner != nil {
		if err := s.miner.StartLocalMining(); err != nil {
			return common.Hash{}, err
		}
	}
	log.Info("Initialized milestone 1 root", "hash", hash, "mining", s.miner != nil)
	return hash, nil
}

// ms1ClusterClient acknowledges calls without forwarding deposits or reporting
// headers to a master. This stub only supports local mining tests; cross-shard
// delivery and root-chain confirmation are not exercised.
type ms1ClusterClient struct{}

func (ms1ClusterClient) BroadcastXShardTxList(ctx context.Context, _ *big.Int, _ common.Hash, _ []*types.DepositTx) error {
	return ctx.Err()
}

func (ms1ClusterClient) SendMinorBlockHeaderToMaster(ctx context.Context, _ *types.Header, _ *uint256.Int) error {
	return ctx.Err()
}
