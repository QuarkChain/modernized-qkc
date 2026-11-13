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

// Temporary milestone 1 master: initialize --m1 geth nodes with an offline R0
// and receive their shard headers over the real cluster protocol.
// TODO(m1): Replace this driver when root-chain and mining control are available.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/qkc/masterconn"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
	"github.com/naoina/toml"
	"github.com/urfave/cli/v2"
)

var m1Command = &cli.Command{
	Name:  "m1",
	Usage: "Initialize milestone 1 shards and watch their reported block heights",
	Flags: []cli.Flag{
		&cli.PathFlag{Name: "root", Value: "qkc/config/devnet/root.toml", Usage: "Root genesis TOML fixture"},
		&cli.StringSliceFlag{Name: "config", Value: cli.NewStringSlice(
			"qkc/config/devnet/shard-100001.toml",
			"qkc/config/devnet/shard-100002.toml",
		), Usage: "Shard TOML containing the cluster endpoint and chain ID (repeat for each shard)"},
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

// Read the same node and genesis settings as geth; do not hardcode TCP endpoints.
func loadM1Slave(path string) (comm.SlaveInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return comm.SlaveInfo{}, err
	}
	var cfg struct {
		Node node.Config
		Eth  ethconfig.Config
	}
	decoder := toml.Config{NormFieldName: func(_ reflect.Type, key string) string { return key }}
	if err := decoder.Unmarshal(data, &cfg); err != nil {
		return comm.SlaveInfo{}, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Node.PrivateHost == "" || cfg.Node.PrivatePort <= 0 || cfg.Node.PrivatePort > 65535 {
		return comm.SlaveInfo{}, fmt.Errorf("%s: missing or invalid cluster endpoint", path)
	}
	if cfg.Eth.Genesis == nil || cfg.Eth.Genesis.Config == nil || cfg.Eth.Genesis.Config.ChainID == nil {
		return comm.SlaveInfo{}, fmt.Errorf("%s: missing genesis chain ID", path)
	}
	id := cfg.Eth.Genesis.Config.ChainID
	if id.Sign() <= 0 || id.BitLen() > 256 {
		return comm.SlaveInfo{}, fmt.Errorf("%s: invalid genesis chain ID", path)
	}
	return comm.SlaveInfo{Host: cfg.Node.PrivateHost, Port: uint16(cfg.Node.PrivatePort), ChainID: id}, nil
}

// m1Master only records reported heights. It does not construct or confirm R1.
type m1Master struct {
	mu      sync.Mutex
	root    common.Hash
	heights map[string]uint64
}

func (m *m1Master) AddShardBlockHeader(source *big.Int, header *types.Header, _ *uint256.Int) error {
	extra, err := header.QKCExtra()
	if err != nil {
		return err
	}
	if extra.ChainID.Cmp(source) != 0 || extra.PrevRootBlock != m.root || header.Number == nil || !header.Number.IsUint64() {
		return errors.New("unexpected milestone 1 shard header")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.heights[source.String()]; !ok {
		return fmt.Errorf("unexpected shard %s", source)
	}
	m.heights[source.String()] = header.Number.Uint64()
	return nil
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
	rootRelation := fmt.Sprintf("root: %d -> [%s]", root.Number(), strings.Join(includedHeaders, ", "))
	master := &m1Master{root: root.Hash(), heights: make(map[string]uint64)}
	var slaves []comm.SlaveInfo
	for _, path := range ctx.StringSlice("config") {
		slave, err := loadM1Slave(path)
		if err != nil {
			return err
		}
		id := slave.ChainID.String()
		if _, exists := master.heights[id]; exists {
			return fmt.Errorf("duplicate shard %s", id)
		}
		master.heights[id] = 0
		slaves = append(slaves, slave)
	}
	if len(slaves) == 0 {
		return errors.New("at least one shard config is required")
	}
	signalCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt)
	defer stop()
	runCtx, shutdown := context.WithCancelCause(signalCtx)
	defer shutdown(nil)
	var peers []*masterconn.SlaveConn
	// Verify every master connection before asking any slave to dial another.
	for _, slave := range slaves {
		endpoint := net.JoinHostPort(slave.Host, strconv.Itoa(int(slave.Port)))
		callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		socket, err := new(net.Dialer).DialContext(callCtx, "tcp", endpoint)
		if err != nil {
			cancel()
			return fmt.Errorf("connect %s: %w", endpoint, err)
		}
		peer, err := masterconn.NewSlaveConn(masterconn.SlaveConnConfig{
			Conn: socket, RemoteChainID: slave.ChainID, Handler: master,
			ShutdownMaster: func() { shutdown(fmt.Errorf("shard %s disconnected", slave.ChainID)) },
		})
		if err != nil {
			cancel()
			socket.Close()
			return err
		}
		defer peer.Close()
		// Closing the socket also interrupts a write if shutdown occurs mid-RPC.
		stopClose := context.AfterFunc(runCtx, peer.Close)
		defer stopClose()
		peer.Start()
		err = peer.SendPing(callCtx, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("chain ID at %s: %w", endpoint, err)
		}
		peers = append(peers, peer)
	}
	// Interconnect one slave at a time, as required by the connection protocol.
	// Empty deposit lists also need delivery ACKs before headers can be reported.
	for _, peer := range peers {
		callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		err := peer.SendConnectToSlaves(callCtx, slaves)
		cancel()
		if err != nil {
			return fmt.Errorf("connect chain %s to slaves: %w", peer.RemoteChainID(), err)
		}
	}
	for _, peer := range peers {
		callCtx, cancel := context.WithTimeout(runCtx, 5*time.Second)
		err := peer.SendPing(callCtx, root)
		cancel()
		if err != nil {
			return fmt.Errorf("initialize chain %s: %w", peer.RemoteChainID(), err)
		}
		fmt.Fprintf(ctx.App.Writer, "chain %s: initialized R0 %s\n", peer.RemoteChainID(), root.Hash())
	}
	fmt.Fprintln(ctx.App.Writer, "Watching reported heights; exiting this command shuts down the shards.")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var heights []string
		master.mu.Lock()
		for _, peer := range peers {
			id := peer.RemoteChainID().String()
			heights = append(heights, fmt.Sprintf("%s=%d", id, master.heights[id]))
		}
		master.mu.Unlock()
		fmt.Fprintf(ctx.App.Writer, "%s | shards: %s\n", rootRelation, strings.Join(heights, "  "))
		select {
		case <-runCtx.Done():
			if errors.Is(context.Cause(runCtx), context.Canceled) {
				return nil
			}
			return context.Cause(runCtx)
		case <-ticker.C:
		}
	}
}
