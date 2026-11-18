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

import (
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/ethereum/go-ethereum/common"
	posw "github.com/ethereum/go-ethereum/consensus/poswshard"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfave/cli/v2"
)

var mineCommand = &cli.Command{
	Name:  "mine",
	Usage: "Mine and submit one shard block using a CPU Ethash test client",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "rpc", Value: "http://127.0.0.1:18545", Usage: "Shard mining RPC endpoint"},
		&cli.StringFlag{Name: "coinbase", Value: "0x2222222222222222222222222222222222222222", Usage: "Mining reward recipient"},
	},
	Action: runMine,
}

func runMine(ctx *cli.Context) error {
	if ctx.NArg() != 0 {
		return errors.New("mine takes no positional arguments")
	}
	if !common.IsHexAddress(ctx.String("coinbase")) {
		return errors.New("invalid coinbase address")
	}
	signalCtx, stop := signal.NotifyContext(ctx.Context, os.Interrupt)
	defer stop()
	client, err := rpc.DialContext(signalCtx, ctx.String("rpc"))
	if err != nil {
		return err
	}
	defer client.Close()
	var work shard.MiningWork
	if err := client.CallContext(signalCtx, &work, "qkc_getWork", common.HexToAddress(ctx.String("coinbase"))); err != nil {
		return fmt.Errorf("get work: %w", err)
	}
	if work.Difficulty == nil || work.Difficulty.Sign() <= 0 {
		return errors.New("work has non-positive difficulty")
	}
	fmt.Fprintf(ctx.App.Writer, "work: number=%d sealhash=%s difficulty=%s\n", work.Number, work.SealHash.TerminalString(), work.Difficulty)
	// SearchNonce only needs the Ethash cache; mining parameters come from RPC.
	pow := &posw.PoSW{EthashVerifier: posw.NewEthashVerifier()}
	defer pow.Close()
	nonce, mixHash, ok := pow.SearchNonce(signalCtx, work.Number, work.SealHash, work.Difficulty, 0)
	if !ok {
		return signalCtx.Err()
	}
	fmt.Fprintf(ctx.App.Writer, "seal: nonce=%d mixhash=%s\n", nonce.Uint64(), mixHash.TerminalString())
	var accepted bool
	if err := client.CallContext(signalCtx, &accepted, "qkc_submitWork", work.SealHash, nonce, mixHash); err != nil {
		return fmt.Errorf("submit work: %w", err)
	}
	if !accepted {
		return errors.New("mining work was rejected")
	}
	fmt.Fprintf(ctx.App.Writer, "accepted: number=%d sealhash=%s\n", work.Number, work.SealHash.TerminalString())
	return nil
}
