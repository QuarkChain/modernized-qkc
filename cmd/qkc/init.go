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
	"fmt"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/node"
	qkcconfig "github.com/ethereum/go-ethereum/qkc/config"
	"github.com/ethereum/go-ethereum/qkc/genesis"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/urfave/cli/v2"
)

var initQKCCommand = &cli.Command{
	Name:   "init",
	Usage:  "Initialize QKC databases from the deployment's genesis files",
	Flags:  []cli.Flag{configFileFlag, qkcRoleFlag, qkcChainFlag, utils.DataDirFlag, utils.DBEngineFlag, utils.StateSchemeFlag},
	Action: initQKC,
}

func initQKC(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("init takes no positional arguments; set GenesisFile in the deployment")
	}
	var cluster *qkcconfig.ClusterConfig
	var selected *qkcconfig.NodeConfig
	var err error
	if ctx.IsSet(qkcRoleFlag.Name) {
		selected, cluster, err = loadQKCConfig(ctx)
	} else {
		if ctx.IsSet(qkcChainFlag.Name) || ctx.IsSet(utils.DataDirFlag.Name) {
			return errors.New("--qkc.chain and --datadir require --qkc.role when initializing one process")
		}
		cluster, err = qkcconfig.LoadCluster(ctx.String(configFileFlag.Name))
	}
	if err != nil {
		return err
	}
	configs := []*qkcconfig.NodeConfig{&cluster.Master}
	for i := range cluster.Slaves {
		configs = append(configs, &cluster.Slaves[i])
	}
	for _, cfg := range configs {
		if ctx.IsSet(utils.DBEngineFlag.Name) {
			cfg.Node.DBEngine = ctx.String(utils.DBEngineFlag.Name)
		}
		if cfg.Eth != nil && ctx.IsSet(utils.StateSchemeFlag.Name) {
			cfg.Eth.StateScheme = ctx.String(utils.StateSchemeFlag.Name)
		}
	}
	var bundle *genesis.Bundle
	if selected != nil && selected.Role == qkcconfig.RoleSlave {
		selected.Eth.Genesis, err = genesis.LoadShard(selected.GenesisFile)
		if err != nil {
			return err
		}
		if err := selected.Validate(cluster.NetworkID); err != nil {
			return err
		}
	} else {
		// Master needs every G0 to construct R0. Whole-deployment initialization
		// also validates cross-shard rules before processing individual nodes.
		if err := cluster.LoadGenesis(); err != nil {
			return err
		}
		if err := cluster.Validate(); err != nil {
			return err
		}
		bundle, err = genesis.Load(cluster)
		if err != nil {
			return err
		}
		cluster.Master.Root = bundle.RootConfig
		for i := range cluster.Slaves {
			for _, shard := range bundle.Shards {
				if cluster.Slaves[i].ChainID.Cmp(shard.Genesis.Config.ChainID) == 0 {
					cluster.Slaves[i].Eth.Genesis = shard.Genesis
					break
				}
			}
		}
	}
	if selected != nil {
		configs = []*qkcconfig.NodeConfig{selected}
	}
	for _, cfg := range configs {
		if cfg.Node.DataDir == "" {
			return fmt.Errorf("%s: init requires a persistent DataDir", cfg.Node.Name)
		}
	}
	// Each process uses the same initializer whether selected individually or
	// in a deployment. If a later process fails, earlier databases remain valid
	// and the command can be rerun, just like repeating geth init.
	for _, cfg := range configs {
		hash, err := initQKCProcess(cfg, bundle)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.Node.Name, err)
		}
		if _, err := fmt.Fprintf(ctx.App.Writer, "Initialized %s (network ID %d, genesis %s)\n", cfg.Node.Name, cluster.NetworkID, hash); err != nil {
			return err
		}
	}
	return nil
}

func initQKCProcess(cfg *qkcconfig.NodeConfig, bundle *genesis.Bundle) (hash common.Hash, err error) {
	stack, err := node.New(&cfg.Node)
	if err != nil {
		return common.Hash{}, err
	}
	defer func() { err = errors.Join(err, stack.Close()) }()
	name := "rootdata"
	options := node.DatabaseOptions{Cache: 128, Handles: 128}
	if cfg.Eth != nil {
		name = "chaindata"
		options.Cache = cfg.Eth.DatabaseCache
		if cfg.Eth.DatabaseHandles > 0 {
			options.Handles = cfg.Eth.DatabaseHandles
		}
		options.AncientsDirectory = cfg.Eth.DatabaseFreezer
		options.EraDirectory = cfg.Eth.DatabaseEra
	}
	db, err := stack.OpenDatabaseWithOptions(name, options)
	if err != nil {
		return common.Hash{}, err
	}
	if cfg.Role == qkcconfig.RoleMaster {
		return bundle.InitRootDatabase(db)
	}
	scheme, err := rawdb.ParseStateScheme(cfg.Eth.StateScheme, db)
	if err != nil {
		return common.Hash{}, err
	}
	trieConfig := &triedb.Config{Preimages: cfg.Eth.Preimages}
	if scheme == rawdb.HashScheme {
		trieConfig.HashDB = hashdb.Defaults
	} else {
		pathConfig := *pathdb.Defaults
		pathConfig.JournalDirectory = stack.ResolvePath("triedb")
		trieConfig.PathDB = &pathConfig
	}
	trie := triedb.NewDatabase(db, trieConfig)
	_, hash, compatErr, err := core.SetupGenesisBlock(db, trie, cfg.Eth.Genesis)
	if compatErr != nil {
		err = errors.Join(err, compatErr)
	}
	if err = errors.Join(err, trie.Close()); err != nil {
		return common.Hash{}, err
	}
	return hash, db.SyncKeyValue()
}
