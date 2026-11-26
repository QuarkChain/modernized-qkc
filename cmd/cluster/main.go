// Copyright 2026 The quarkchain Authors
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

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/master"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet/masternet"
	"github.com/urfave/cli/v2"
)

const devnetConfig = "qkc/config/devnet/root.toml"

func main() {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, false)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newApp().RunContext(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	app := flags.NewApp("Start a local QuarkChain root node and its shards")
	app.Name = "cluster"
	app.Flags = []cli.Flag{
		&cli.BoolFlag{Name: "devnet", Usage: "Use " + devnetConfig + " from the working directory"},
		&cli.PathFlag{Name: "config", Usage: "Use the specified root TOML"},
	}
	app.Commands = []*cli.Command{{
		Name:  "shard",
		Usage: "Start a shard from its geth TOML",
		Flags: []cli.Flag{&cli.PathFlag{Name: "config", Usage: "Shard TOML", Required: true}},
		Action: func(ctx *cli.Context) error {
			if ctx.NArg() != 0 {
				return errors.New("shard takes no positional arguments")
			}
			cfg, err := config.LoadShard(ctx.Path("config"))
			if err != nil {
				return fmt.Errorf("load shard config: %w", err)
			}
			return runShard(ctx.Context, cfg)
		},
	}}
	app.Action = func(ctx *cli.Context) error {
		path := ctx.Path("config")
		if ctx.NArg() != 0 || ctx.Bool("devnet") == (path != "") {
			return errors.New("specify either --devnet or --config <root.toml>")
		}
		if ctx.Bool("devnet") {
			path = devnetConfig
		}
		cfg, err := config.LoadRoot(path)
		if err != nil {
			return fmt.Errorf("load cluster config: %w", err)
		}
		return runCluster(ctx.Context, cfg, ctx.App.Writer)
	}
	return app
}

func runCluster(ctx context.Context, cfg *config.RootConfig, writer io.Writer) (err error) {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate cluster executable: %w", err)
	}
	// The parent closes root before signaling its children to exit.
	shardCtx, stopShards := context.WithCancel(context.Background())
	var shards sync.WaitGroup
	defer func() {
		stopShards()
		shards.Wait()
	}()

	stopped := make(chan error, len(cfg.Shards))
	slaves := make([]clusternet.SlaveInfo, 0, len(cfg.Shards))
	for _, shard := range cfg.Shards {
		if ctx.Err() != nil {
			return nil
		}
		chainID := shard.Slave.ChainID.String()
		child := exec.CommandContext(shardCtx, executable, "shard", "--config", shard.Path)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.Cancel = func() error { return child.Process.Signal(syscall.SIGTERM) }
		child.WaitDelay = 10 * time.Second
		if err := child.Start(); err != nil {
			return fmt.Errorf("start shard %s process: %w", chainID, err)
		}
		log.Info("Started shard process", "chain", chainID, "pid", child.Process.Pid)
		shards.Go(func() {
			if err := child.Wait(); err != nil {
				stopped <- fmt.Errorf("shard %s process stopped: %w", chainID, err)
				return
			}
			stopped <- fmt.Errorf("shard %s process stopped unexpectedly", chainID)
		})
		slaves = append(slaves, shard.Slave)
	}

	if ctx.Err() != nil {
		return nil
	}
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return fmt.Errorf("create root node: %w", err)
	}
	defer func() {
		if closeErr := stack.Close(); closeErr != nil && !errors.Is(closeErr, node.ErrNodeStopped) {
			err = errors.Join(err, closeErr)
		}
	}()
	backend, err := master.New(stack, &master.Config{
		Genesis:      cfg.Genesis,
		ShardGeneses: cfg.ShardGeneses(),
		Cluster:      masternet.ServiceConfig{SlaveInfos: slaves},
	})
	if err != nil {
		return fmt.Errorf("create root backend: %w", err)
	}
	if err := stack.Start(); err != nil {
		return fmt.Errorf("start root: %w", err)
	}
	rootStopped := make(chan struct{})
	go func() {
		stack.Wait()
		close(rootStopped)
	}()

	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	ready := poll.C
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-stopped:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-rootStopped:
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("root node stopped unexpectedly")
		case <-ready:
			if backend.Ready() {
				if _, err := fmt.Fprintln(writer, "Cluster ready"); err != nil {
					return err
				}
				poll.Stop()
				ready = nil
			}
		}
	}
}

func runShard(ctx context.Context, cfg config.ShardConfig) (err error) {
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return fmt.Errorf("create shard node: %w", err)
	}
	defer func() {
		if closeErr := stack.Close(); closeErr != nil && !errors.Is(closeErr, node.ErrNodeStopped) {
			err = errors.Join(err, closeErr)
		}
	}()
	backend, err := eth.New(stack, &cfg.Eth)
	if err != nil {
		return fmt.Errorf("create shard backend: %w", err)
	}
	utils.RegisterFilterAPI(stack, backend.APIBackend, &cfg.Eth)
	if err := stack.Start(); err != nil {
		return fmt.Errorf("start shard: %w", err)
	}
	stopped := make(chan struct{})
	go func() {
		stack.Wait()
		close(stopped)
	}()
	select {
	case <-ctx.Done():
	case <-stopped:
	}
	return nil
}
