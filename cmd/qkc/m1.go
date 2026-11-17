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

// Temporary milestone 1 driver: start local shards, relay deposits, and confirm
// mined shard blocks with offline roots while external miners seal work over RPC.
// TODO(m1): Replace this driver when root-chain and mining control are available.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

var m1Command = &cli.Command{
	Name:  "m1",
	Usage: "Start local shards with deposit delivery and offline root confirmations",
	Flags: []cli.Flag{
		&cli.StringSliceFlag{Name: "config", Value: cli.NewStringSlice(
			"qkc/config/devnet/shard-100001.toml",
		), Usage: "Native geth shard TOML (repeat for each shard)"},
	},
	Action: runM1,
}

// startM1Shard uses the ordinary Ethereum backend. The driver coordinates the
// started shards directly while their cluster clients continue to log calls.
func startM1Shard(path string) (*node.Node, *eth.Ethereum, error) {
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
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return nil, nil, err
	}
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
		headers = append(headers, backend.BlockChain().Genesis().Header())
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
	paths := ctx.StringSlice("config")
	var stacks []*node.Node
	var backends []*eth.Ethereum
	for _, path := range paths {
		stack, backend, err := startM1Shard(path)
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
	for i, stack := range stacks {
		fmt.Fprintf(ctx.App.Writer, "%s: initialized R0 %s; HTTP %s\n", paths[i], root.Hash(), stack.HTTPEndpoint())
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-signalCtx.Done():
			return nil
		case <-ticker.C:
			next, err := advanceM1Root(signalCtx, backends, root)
			if err != nil {
				if signalCtx.Err() != nil {
					return nil
				}
				return err
			}
			if next.Hash() != root.Hash() {
				fmt.Fprintf(ctx.App.Writer, "R%d %s: confirmed %d shard blocks after deposit delivery\n", next.Number(), next.Hash(), len(next.ShardBlockHeaders()))
				root = next
			}
		}
	}
}
