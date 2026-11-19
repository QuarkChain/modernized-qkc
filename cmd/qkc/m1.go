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

// MILESTONE 1 TEST ONLY.
// This driver starts local shards and initializes an offline R0 so external
// miners can fetch work and submit seals over RPC.
// TODO(m1): Replace this driver when root-chain and mining control are available.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

var m1Command = &cli.Command{
	Name:  "m1",
	Usage: "Start local shards with manually triggered offline root confirmations",
	Flags: []cli.Flag{
		&cli.StringSliceFlag{Name: "config", Value: cli.NewStringSlice(
			"qkc/config/devnet/shard-100001.toml",
		), Usage: "Native geth shard TOML (repeat for each shard)"},
	},
	Action:      runM1,
	Subcommands: []*cli.Command{m1RootCommand},
}

var m1RootCommand = &cli.Command{
	Name:  "root",
	Usage: "Deliver pending deposits and advance the running m1 driver's root once",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "rpc", Value: "http://127.0.0.1:18545", Usage: "An m1 shard's RPC endpoint"},
	},
	Action: runM1Root,
}

func runM1Root(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("m1 root takes no positional arguments")
	}
	client, err := rpc.DialContext(ctx.Context, ctx.String("rpc"))
	if err != nil {
		return err
	}
	defer client.Close()
	var header qkctypes.RootBlockHeader
	if err := client.CallContext(ctx.Context, &header, "m1_advanceRoot"); err != nil {
		return fmt.Errorf("advance root: %w", err)
	}
	fmt.Fprintf(ctx.App.Writer, "root tip: R%d %s\n", header.Number, header.Hash())
	return nil
}

// m1RootAPI serializes manual root commands across all started shards.
type m1RootAPI struct {
	mu       sync.Mutex
	ctx      context.Context
	backends []*eth.Ethereum
	root     *qkctypes.RootBlock
	err      error
}

func (api *m1RootAPI) AdvanceRoot(ctx context.Context) (*qkctypes.RootBlockHeader, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := api.ctx.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if api.root == nil {
		return nil, errors.New("m1 shards are not initialized")
	}
	if api.err != nil {
		return nil, api.err
	}
	next, err := advanceM1Root(ctx, api.backends, api.root)
	if err != nil {
		// Some shards may already have accepted the root. Stop advancement so a
		// later command cannot build on inconsistent checkpoints.
		api.err = fmt.Errorf("root advancement stopped; restart m1 with fresh data directories: %w", err)
		return nil, api.err
	}
	api.root = next
	return next.Header(), nil
}

// startM1Shard uses the ordinary Ethereum backend. The driver coordinates the
// started shards directly while their cluster clients continue to log calls.
func startM1Shard(path string, apis ...rpc.API) (*node.Node, *eth.Ethereum, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg := struct {
		Node node.Config
		Eth  ethconfig.Config
	}{Node: node.DefaultConfig, Eth: ethconfig.Defaults}
	cfg.Node.Name = "geth"
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Eth.Genesis == nil || cfg.Eth.Genesis.Config == nil || cfg.Eth.Genesis.Config.QuarkChain == nil {
		return nil, nil, fmt.Errorf("%s: missing shard genesis", path)
	}
	if err := cfg.Eth.Genesis.ValidateQuarkChain(); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid genesis: %w", path, err)
	}
	for _, api := range apis {
		cfg.Node.HTTPModules = append(cfg.Node.HTTPModules, api.Namespace)
	}
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return nil, nil, err
	}
	stack.RegisterAPIs(apis)
	backend, err := eth.New(stack, &cfg.Eth)
	if err != nil {
		stack.Close()
		return nil, nil, err
	}
	if err := stack.Start(); err != nil {
		stack.Close()
		return nil, nil, err
	}
	return stack, backend, nil
}

func initM1Shards(backends []*eth.Ethereum) (*qkctypes.RootBlock, error) {
	var headers qkctypes.ShardBlockHeaders
	for _, backend := range backends {
		chain := backend.BlockChain()
		head := chain.CurrentBlock()
		if head.Number.Sign() != 0 {
			return nil, fmt.Errorf("shard %s is at block %s; use a fresh data directory for qkc m1", chain.Config().ChainID, head.Number)
		}
		headers = append(headers, head)
	}
	root := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Difficulty: *uint256.NewInt(1), TotalDifficulty: *uint256.NewInt(1),
	}, headers, nil)
	for _, backend := range backends {
		if err := backend.Shard().Init(root); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// advanceM1Root delivers all pending blocks' deposits before confirming their
// headers in a new offline root. Empty lists are delivered too. Roots have unit
// difficulty and no rewards or seals; this driver only coordinates local shards.
func advanceM1Root(ctx context.Context, backends []*eth.Ethereum, root *qkctypes.RootBlock) (*qkctypes.RootBlock, error) {
	backends = slices.Clone(backends)
	slices.SortFunc(backends, func(a, b *eth.Ethereum) int {
		return a.BlockChain().Config().ChainID.Cmp(b.BlockChain().Config().ChainID)
	})
	var headers qkctypes.ShardBlockHeaders
	timestamp := max(uint64(time.Now().Unix()), root.Time()+1)
	for _, source := range backends {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pending, err := source.Shard().GetUnconfirmedHeaderList()
		if err != nil {
			return nil, fmt.Errorf("collect shard %s headers: %w", source.BlockChain().Config().ChainID, err)
		}
		for _, header := range pending {
			deposits, err := shard.XShardDeposits(source.BlockChain(), header.Hash())
			if err != nil {
				return nil, err
			}
			for _, destination := range backends {
				if source == destination {
					continue
				}
				chainID := destination.BlockChain().Config().ChainID
				list, ok := deposits[chainID.String()]
				if !ok {
					return nil, fmt.Errorf("shard %s is outside source shard %s's network", chainID, source.BlockChain().Config().ChainID)
				}
				if err := destination.Shard().AddXshardTxList(header.Hash(), list); err != nil {
					return nil, fmt.Errorf("deliver block %s deposits to shard %s: %w", header.Hash(), chainID, err)
				}
			}
			headers = append(headers, header)
			timestamp = max(timestamp, header.Time)
		}
	}
	if len(headers) == 0 {
		return root, nil
	}
	next := qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Number: root.Number() + 1, ParentHash: root.Hash(), Time: timestamp,
		Difficulty: *uint256.NewInt(1), TotalDifficulty: *new(uint256.Int).Add(root.TotalDifficulty(), uint256.NewInt(1)),
	}, headers, nil)
	for _, backend := range backends {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := backend.Shard().AddRootBlock(next); err != nil {
			return nil, fmt.Errorf("insert R%d on shard %s: %w", next.Number(), backend.BlockChain().Config().ChainID, err)
		}
	}
	return next, nil
}

func runM1(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("m1 takes no positional arguments")
	}
	signalCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt)
	defer stop()
	control := &m1RootAPI{ctx: signalCtx}
	paths := ctx.StringSlice("config")
	var stacks []*node.Node
	var backends []*eth.Ethereum
	for _, path := range paths {
		stack, backend, err := startM1Shard(path, rpc.API{Namespace: "m1", Service: control})
		if err != nil {
			return fmt.Errorf("start %s: %w", path, err)
		}
		defer stack.Close()
		stacks = append(stacks, stack)
		backends = append(backends, backend)
	}
	root, err := initM1Shards(backends)
	if err != nil {
		return err
	}
	control.mu.Lock()
	control.backends, control.root = backends, root
	control.mu.Unlock()
	for i, stack := range stacks {
		fmt.Fprintf(ctx.App.Writer, "%s: initialized R0 %s; HTTP %s\n", paths[i], root.Hash(), stack.HTTPEndpoint())
	}
	<-signalCtx.Done()
	// Wait for a root command to finish before closing any of its shard backends.
	// New calls observe signalCtx cancellation before touching those backends.
	control.mu.Lock()
	control.mu.Unlock()
	return nil
}
