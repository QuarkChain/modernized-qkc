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

// Package master owns the root node's business components and lifecycle.
package master

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/consensus/poswroot"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/clusternet/masternet"
	"github.com/ethereum/go-ethereum/qkc/root"
	"github.com/ethereum/go-ethereum/rpc"
)

// Config is temporary and will be revised when configuration is consolidated.
// Chain must match the existing root database and remain immutable after New,
// as must Cluster's endpoint slices and chain IDs. Backend supplies Cluster's
// ConfiguredChainIDs and Shutdown from Chain and its own lifecycle respectively.
//
// TODO(config): Update Config once the configuration is finalized.
type Config struct {
	Chain    *params.QuarkChainConfig
	Cluster  masternet.ServiceConfig
	Database node.DatabaseOptions
}

// Backend owns the root chain, engine, database and cluster service. Node owns
// the external endpoints and serializes Start/Stop through its lifecycle.
// Callers close the node, not this backend directly. All access to business
// resources must start after Start and finish before Stop returns.
type Backend struct {
	stack   *node.Node
	ctx     context.Context
	cancel  context.CancelFunc
	cluster *masternet.Service
	chain   *root.RootChain
	engine  poswroot.RootEngine
	db      ethdb.Database
	logger  log.Logger

	initTasks    sync.WaitGroup
	shutdownOnce sync.Once
}

var (
	_ node.Lifecycle          = (*Backend)(nil)
	_ masternet.MasterBackend = (*root.RootChain)(nil)
	_ root.ClusterClient      = (*masternet.Service)(nil)
)

// New recovers an existing root database. Genesis setup is deliberately absent:
// a missing head or confirmation snapshot fails construction without seeding it.
// No cluster traffic or business goroutines run until Start.
func New(stack *node.Node, cfg *Config) (_ *Backend, err error) {
	if stack == nil || cfg == nil || cfg.Chain == nil {
		return nil, errors.New("missing node or master configuration")
	}
	if cfg.Database.ReadOnly {
		return nil, errors.New("master requires a writable root database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Backend{stack: stack, ctx: ctx, cancel: cancel, logger: stack.Config().Logger}
	defer func() {
		if err != nil {
			// No lifecycle or background tasks have been registered yet.
			// Release construction resources without calling lifecycle Stop.
			cancel()
			if b.engine != nil {
				err = errors.Join(err, b.engine.Close())
			}
			if b.db != nil {
				err = errors.Join(err, b.db.Close())
			}
		}
	}()
	if cfg.Chain.ConsensusType == params.ConsensusSimulate {
		b.engine, err = poswroot.NewSimulate(cfg.Chain)
	} else {
		b.engine, err = poswroot.New(cfg.Chain)
	}
	if err != nil {
		return nil, err
	}
	clusterCfg := cfg.Cluster
	clusterCfg.ConfiguredChainIDs = cfg.Chain.ChainIDs
	clusterCfg.Shutdown = b.requestShutdown
	if clusterCfg.Logger == nil {
		clusterCfg.Logger = b.logger
	}
	b.cluster, err = masternet.NewService(clusterCfg)
	if err != nil {
		return nil, err
	}
	dbOptions := cfg.Database
	if dbOptions.MetricsNamespace == "" {
		dbOptions.MetricsNamespace = "qkc/db/master/"
	}
	// Node resolves DataDir to an absolute path. Use it directly as the root DB
	// directory, without appending an instance name or another database directory.
	b.db, err = stack.OpenDatabaseWithOptions(stack.Config().DataDir, dbOptions)
	if err != nil {
		return nil, err
	}
	// TODO(genesis): Call SetupRootGenesisBlock here once it is checked in.
	b.chain, err = root.New(ctx, cfg.Chain, b.cluster, b.db, b.engine)
	if err != nil {
		return nil, fmt.Errorf("recover root chain (initialized root database required): %w", err)
	}
	stack.RegisterAPIs(b.APIs())
	stack.RegisterProtocols(b.Protocols())
	stack.RegisterLifecycle(b)
	return b, nil
}

// Start launches initialization without waiting for the cluster. Node.Start
// holds its startStopLock until this method returns, and Node.Close needs the
// same lock before it can call Stop. Waiting here for an unavailable shard
// would prevent shutdown from stopping the service's connection retries.
// Node serializes lifecycle calls, so task registration finishes before Stop
// can wait on it; the backend needs no separate lifecycle lock or state machine.
func (b *Backend) Start() error {
	if err := b.cluster.Start(b.chain); err != nil {
		return err
	}
	b.initTasks.Add(1)
	go b.initialize()
	return nil
}

func (b *Backend) initialize() {
	defer b.initTasks.Done()
	var err error
	select {
	case <-b.ctx.Done():
		return
	case <-b.cluster.WaitForInitialization():
		if !b.cluster.IsActive() {
			err = errors.New("master cluster initialization failed")
		} else {
			err = b.chain.Init()
		}
	}
	if err != nil && b.ctx.Err() == nil {
		b.logger.Error("Master initialization failed", "err", err)
		b.requestShutdown()
	}
}

// Ready includes shutdown cancellation as well as shard checkpoint initialization.
func (b *Backend) Ready() bool { return b.chain.Ready() }

// Stop closes admission before canceling work. Service.Stop releases network
// waits and drains report handlers before chain/engine/database resources close.
// Node calls it once for a successfully started backend, as for eth.Ethereum;
// it is not an unstarted-instance cleanup or a concurrent/repeated-call API.
func (b *Backend) Stop() error {
	b.cancel()
	err := b.cluster.Stop()
	b.initTasks.Wait()
	b.chain.Close()
	return errors.Join(err, b.engine.Close(), b.db.Close())
}

// Never wait here: Service or initialization may invoke this callback while
// Stop is waiting for that very operation to finish.
func (b *Backend) requestShutdown() {
	b.shutdownOnce.Do(func() {
		b.cancel()
		go func() {
			if err := b.stack.Close(); err != nil && !errors.Is(err, node.ErrNodeStopped) {
				b.logger.Error("Failed to close master node", "err", err)
			}
		}()
	})
}

// RootChain and Engine are borrowed components. Callers must honor Ready and
// coordinate their work with the backend lifecycle; they must not close them.
func (b *Backend) RootChain() *root.RootChain  { return b.chain }
func (b *Backend) Engine() poswroot.RootEngine { return b.engine }
func (b *Backend) APIs() []rpc.API             { return nil }
func (b *Backend) Protocols() []p2p.Protocol   { return nil }
