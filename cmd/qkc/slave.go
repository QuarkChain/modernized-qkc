// Copyright 2026-2027 The quarkchain Authors
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

import (
	"errors"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/slave"
	"github.com/urfave/cli/v2"
)

func makeSlaveNode(ctx *cli.Context) (*node.Node, error) {
	cfg, cluster, err := loadQKCConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Role != config.RoleSlave {
		return nil, errors.New("only --qkc.role slave startup is implemented")
	}
	if err := cfg.LoadGenesis(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(cluster.NetworkID); err != nil {
		return nil, err
	}
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return nil, err
	}
	if _, err := slave.New(stack, cfg.Eth); err != nil {
		stack.Close()
		return nil, err
	}
	return stack, nil
}

func runQKC(ctx *cli.Context) error {
	stack, err := makeSlaveNode(ctx)
	if err != nil {
		return err
	}
	utils.StartNode(ctx, stack, false)
	stack.Wait()
	return nil
}
