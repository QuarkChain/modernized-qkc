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

// Temporary milestone 1 driver: start local shards and initialize an offline R0
// so external miners can fetch work and submit seals over RPC.
// TODO(m1): Replace this driver when root-chain and mining control are available.

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

var m1Command = &cli.Command{
	Name:  "m1",
	Usage: "Start local shards initialized with R0 for external mining",
	Flags: []cli.Flag{
		&cli.PathFlag{Name: "root", Value: "qkc/config/devnet/root.toml", Usage: "Root genesis TOML fixture"},
		&cli.StringSliceFlag{Name: "config", Value: cli.NewStringSlice(
			"qkc/config/devnet/shard-100001.toml",
			"qkc/config/devnet/shard-100002.toml",
		), Usage: "Native geth shard TOML (repeat for each shard)"},
	},
	Action: runM1,
}

func loadM1Root(path string) (*qkctypes.RootBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fixture struct {
		Hash              common.Hash
		Header            *qkctypes.RootBlockHeader
		ShardBlockHeaders qkctypes.ShardBlockHeaders
		TrackingData      []byte
	}
	// Match geth's case-sensitive Go field names.
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.Unmarshal(data, &fixture); err != nil {
		return nil, err
	}
	if fixture.Header == nil || fixture.Header.Number != 0 || fixture.Header.ParentHash != (common.Hash{}) || len(fixture.ShardBlockHeaders) == 0 {
		return nil, errors.New("expected a complete root genesis fixture")
	}
	for _, header := range fixture.ShardBlockHeaders {
		if header == nil || header.Number == nil || header.Number.Sign() != 0 {
			return nil, errors.New("root genesis must contain shard genesis headers")
		}
	}
	// NewRootBlock recomputes this commitment, so check it before construction.
	if fixture.Header.ShardHeaderHash != qkctypes.CalcShardHeaderHash(fixture.ShardBlockHeaders) {
		return nil, errors.New("root genesis body commitment mismatch")
	}
	root := qkctypes.NewRootBlock(fixture.Header, fixture.ShardBlockHeaders, fixture.TrackingData)
	if root.Hash() != fixture.Hash {
		return nil, errors.New("root genesis hash mismatch")
	}
	return root, nil
}

// startM1Shard uses the ordinary Ethereum backend, including its mock cluster
// client. This local demo does not coordinate shards or produce root blocks.
func startM1Shard(path string, root *qkctypes.RootBlock) (*node.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := struct {
		Node node.Config
		Eth  ethconfig.Config
	}{Node: node.DefaultConfig, Eth: ethconfig.Defaults}
	cfg.Node.Name = "geth"
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Eth.Genesis == nil || cfg.Eth.Genesis.Config == nil || cfg.Eth.Genesis.Config.QuarkChain == nil {
		return nil, fmt.Errorf("%s: missing shard genesis", path)
	}
	if err := cfg.Eth.Genesis.ValidateQuarkChain(); err != nil {
		return nil, fmt.Errorf("%s: invalid genesis: %w", path, err)
	}
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return nil, err
	}
	backend, err := eth.New(stack, &cfg.Eth)
	if err != nil {
		stack.Close()
		return nil, err
	}
	if err := stack.Start(); err != nil {
		stack.Close()
		return nil, err
	}
	if err := backend.Shard().Init(root); err != nil {
		stack.Close()
		return nil, err
	}
	return stack, nil
}

func runM1(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("m1 takes no positional arguments")
	}
	root, err := loadM1Root(ctx.Path("root"))
	if err != nil {
		return err
	}
	// R0's included headers remain fixed while the live shard heads advance.
	var includedHeaders []string
	for i, header := range root.ShardBlockHeaders() {
		extra, err := header.QKCExtra()
		if err != nil {
			return fmt.Errorf("decode shard header %d: %w", i, err)
		}
		includedHeaders = append(includedHeaders, fmt.Sprintf("%s@%s", extra.ChainID.String(), header.Number.String()))
	}
	fmt.Fprintf(ctx.App.Writer, "root: %d -> [%s]\n", root.Number(), strings.Join(includedHeaders, ", "))
	signalCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt)
	defer stop()
	for _, path := range ctx.StringSlice("config") {
		stack, err := startM1Shard(path, root)
		if err != nil {
			return fmt.Errorf("start %s: %w", path, err)
		}
		defer stack.Close()
		fmt.Fprintf(ctx.App.Writer, "%s: initialized R0 %s; HTTP %s\n", path, root.Hash(), stack.HTTPEndpoint())
	}
	<-signalCtx.Done()
	return nil
}
