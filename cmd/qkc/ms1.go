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

// Temporary milestone 1 test driver: inject an offline R0 into --ms1 geth nodes
// and watch their block heights without running a master.
// TODO(ms1): Replace the test RPC with master-driven initialization and mining
// control when those services are available.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/node"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/naoina/toml"
	"github.com/naoina/toml/ast"
	"github.com/urfave/cli/v2"
)

var ms1Command = &cli.Command{
	Name:  "ms1",
	Usage: "Inject root genesis into milestone 1 shards and watch their block heights",
	Flags: []cli.Flag{
		&cli.PathFlag{Name: "root", Value: "qkc/config/devnet/root.toml", Usage: "Root genesis TOML fixture"},
		&cli.StringSliceFlag{Name: "ipc", Usage: "Override IPC endpoints read from the devnet shard TOMLs (repeat for each shard)"},
	},
	Action: runMS1,
}

func loadMS1Root(path string) (*qkctypes.RootBlock, error) {
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

// loadMS1IPC reads only the shard's node settings for this temporary demo driver.
// Resolve the endpoint with geth's rules instead of duplicating path handling.
func loadMS1IPC(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	table, err := toml.Parse(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	nodeTable, ok := table.Fields["Node"].(*ast.Table)
	if !ok {
		return "", fmt.Errorf("%s: missing [Node] section", path)
	}
	cfg := node.DefaultConfig
	cfg.IPCPath = "geth.ipc"
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.UnmarshalTable(nodeTable, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	endpoint := cfg.IPCEndpoint()
	if endpoint == "" {
		return "", fmt.Errorf("%s: IPC is disabled", path)
	}
	return endpoint, nil
}

func runMS1(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("ms1 takes no positional arguments")
	}
	root, err := loadMS1Root(ctx.Path("root"))
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
	rootRelation := fmt.Sprintf("root=%d -> shard headers [%s]", root.Number(), strings.Join(includedHeaders, ", "))
	data, err := root.MarshalSSZ()
	if err != nil {
		return err
	}
	endpoints := ctx.StringSlice("ipc")
	if !ctx.IsSet("ipc") {
		for _, path := range []string{
			"qkc/config/devnet/shard-100001.toml",
			"qkc/config/devnet/shard-100002.toml",
		} {
			endpoint, err := loadMS1IPC(path)
			if err != nil {
				return err
			}
			endpoints = append(endpoints, endpoint)
		}
	}
	runCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt)
	defer stop()
	var peers []struct {
		endpoint string
		client   *rpc.Client
		chainID  string
	}
	// Connect to every shard before initializing any of them.
	for _, endpoint := range endpoints {
		callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		client, err := rpc.DialIPC(callCtx, endpoint)
		if err != nil {
			cancel()
			return fmt.Errorf("connect %s: %w", endpoint, err)
		}
		defer client.Close()
		var id hexutil.Big
		err = client.CallContext(callCtx, &id, "eth_chainId")
		cancel()
		if err != nil {
			return fmt.Errorf("chain ID at %s: %w", endpoint, err)
		}
		peers = append(peers, struct {
			endpoint string
			client   *rpc.Client
			chainID  string
		}{endpoint, client, id.ToInt().String()})
	}
	if len(peers) == 0 {
		return errors.New("at least one --ipc endpoint is required")
	}
	for _, peer := range peers {
		callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		var hash common.Hash
		err := peer.client.CallContext(callCtx, &hash, "ms1_initRoot", hexutil.Bytes(data))
		cancel()
		if err != nil {
			return fmt.Errorf("initialize chain %s at %s: %w", peer.chainID, peer.endpoint, err)
		}
		if hash != root.Hash() {
			return fmt.Errorf("chain %s returned a different root hash", peer.chainID)
		}
		fmt.Fprintf(ctx.App.Writer, "chain %s: initialized R0 %s\n", peer.chainID, hash)
	}
	fmt.Fprintln(ctx.App.Writer, "Watching heights; exiting this command leaves the shards running.")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var heights []string
		for _, peer := range peers {
			callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
			var height hexutil.Uint64
			err := peer.client.CallContext(callCtx, &height, "eth_blockNumber")
			cancel()
			if runCtx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read chain %s height: %w", peer.chainID, err)
			}
			heights = append(heights, fmt.Sprintf("%s=%d", peer.chainID, height))
		}
		fmt.Fprintf(ctx.App.Writer, "%s | shard heads: %s\n", rootRelation, strings.Join(heights, "  "))
		select {
		case <-runCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
