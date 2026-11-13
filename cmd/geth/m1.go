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

// Temporary milestone 1 settings. Ethereum owns the normal slave service and
// mining lifecycle; the demo only selects simulated search and disables P2P.

import (
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/urfave/cli/v2"
)

var m1Flag = &cli.BoolFlag{
	Name:  "m1",
	Usage: "Enable milestone 1 automatic simulated mining; disable P2P",
}

func configureM1(ctx *cli.Context, cfg *gethConfig) error {
	if !ctx.Bool(m1Flag.Name) {
		return nil
	}
	if cfg.Node.PrivateHost == "" || cfg.Node.PrivatePort <= 0 || cfg.Node.PrivatePort > 65535 {
		return errors.New("--m1 requires Node.PrivateHost and a valid Node.PrivatePort")
	}
	// The one-second mean search delay is a demo setting.
	cfg.Eth.QKCMiningSearch = shard.NewSimulatedSearch(time.Second)
	cfg.Node.P2P.ListenAddr, cfg.Node.P2P.DiscAddr = "", ""
	cfg.Node.P2P.MaxPeers = 0
	cfg.Node.P2P.NoDial, cfg.Node.P2P.NoDiscovery = true, true
	cfg.Node.P2P.DiscoveryV4, cfg.Node.P2P.DiscoveryV5 = false, false
	return nil
}

// The offline R0 fixture is only for fresh demos. Keep this restriction out of
// the production checkpoint initialization path, which also supports recovery.
func checkM1(backend *eth.Ethereum) error {
	config := backend.BlockChain().Config()
	if config.QuarkChain == nil || config.QuarkChain.ConsensusType != params.ConsensusSimulate {
		return errors.New("--m1 requires a POW_SIMULATE QuarkChain shard")
	}
	if backend.BlockChain().CurrentBlock().Number.Sign() != 0 {
		return errors.New("milestone 1 requires a genesis-only data directory; use a fresh --datadir")
	}
	return nil
}
