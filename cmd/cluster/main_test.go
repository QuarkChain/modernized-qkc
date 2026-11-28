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
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

const clusterTestExecutable = "QKC_CLUSTER_TEST_EXECUTABLE"

func TestMain(m *testing.M) {
	// Run the real CLI when the test launches this binary. The shard processes
	// inherit the marker when the cluster launches a copy of itself.
	if os.Getenv(clusterTestExecutable) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestClusterDevnetRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix process signals")
	}
	dir := t.TempDir()
	configDir := filepath.Join("qkc", "config", "devnet")
	dest := filepath.Join(dir, configDir)
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	type nodeConfig struct {
		file, ipc            string
		privatePort, rpcPort int
	}
	configs := []nodeConfig{
		{"root.toml", "qkc-master.ipc", 18491, 18391},
		{"shard-100001.toml", "qkc-shard-100001.ipc", 38000, 18545},
		{"shard-100002.toml", "qkc-shard-100002.ipc", 38001, 18546},
	}
	ports := reserveClusterPorts(t, 2*len(configs))
	endpoints := make([]string, len(configs))
	for i, cfg := range configs {
		original, err := os.ReadFile(filepath.Join("..", "..", configDir, cfg.file))
		if err != nil {
			t.Fatal(err)
		}
		replacer := strings.NewReplacer(
			fmt.Sprintf("PrivatePort = %d", cfg.privatePort), fmt.Sprintf("PrivatePort = %d", ports[2*i]),
			fmt.Sprintf("HTTPPort = %d", cfg.rpcPort), fmt.Sprintf("HTTPPort = %d", ports[2*i+1]),
			"ListenAddr = \":30303\"", "ListenAddr = \"\"\nNoDial = true",
			fmt.Sprintf("IPCPath = %q", cfg.ipc), "IPCPath = \"\"",
		)
		if err := os.WriteFile(filepath.Join(dest, cfg.file), []byte(replacer.Replace(string(original))), 0600); err != nil {
			t.Fatal(err)
		}
		endpoints[i] = fmt.Sprintf("http://127.0.0.1:%d", ports[2*i+1])
	}
	// The deployment keeps its relative DataDir values and uses the CLI's default
	// devnet location, both resolved from the temporary working directory.
	genesis := checkCluster(t, dir, endpoints, os.Interrupt, "--devnet")
	rootPath := filepath.Join(dest, "root.toml")
	contents, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	withoutGenesis, _, found := strings.Cut(string(contents), "[Root.Genesis]")
	if !found {
		t.Fatal("devnet root TOML has no genesis section")
	}
	if err := os.WriteFile(rootPath, []byte(withoutGenesis), 0600); err != nil {
		t.Fatal(err)
	}
	if restarted := checkCluster(t, dir, endpoints, syscall.SIGTERM, "--config", rootPath); restarted != genesis {
		t.Fatalf("root genesis changed across restart: %s -> %s", genesis, restarted)
	}
	t.Run("unexpected shard exit", func(t *testing.T) {
		checkCluster(t, dir, endpoints, os.Kill, "--config", rootPath)
	})
}

func reserveClusterPorts(t *testing.T, count int) []int {
	t.Helper()
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	var ports []int
	for range count {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports
}

func checkCluster(t *testing.T, dir string, endpoints []string, stop os.Signal, flags ...string) common.Hash {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "cluster-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	logOutput := func() string {
		data, err := os.ReadFile(output.Name())
		if err != nil {
			t.Error(err)
		}
		return string(data)
	}
	cmd := exec.Command(executable, flags...)
	cmd.Env = append(os.Environ(), clusterTestExecutable+"=1")
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = cmd.Wait()
	}()
	shardCrashed := false
	defer func() {
		if !shardCrashed {
			signal := stop
			if signal == os.Kill {
				signal = syscall.SIGTERM
			}
			if err := cmd.Process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
		}
		select {
		case <-done:
			if shardCrashed && cmd.ProcessState.ExitCode() != 1 {
				t.Errorf("cluster exit status after shard failure = %d, want 1", cmd.ProcessState.ExitCode())
			}
			if !shardCrashed && runErr != nil {
				t.Errorf("cluster exited after %v: %v", stop, runErr)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("cluster did not stop after %v", stop)
			cmd.Process.Kill()
			<-done
		}
		logs := logOutput()
		for _, pid := range shardProcessIDs(logs) {
			if err := signalProcess(pid, syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				t.Errorf("shard process %d was not reaped: %v", pid, err)
				if err == nil {
					signalProcess(pid, os.Kill)
				}
			}
		}
		if t.Failed() {
			t.Log(logs)
		}
	}()

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !strings.Contains(logOutput(), "Cluster ready") {
		select {
		case <-done:
			t.Fatalf("cluster exited before readiness: %v", runErr)
		case <-ctx.Done():
			t.Fatal("cluster did not become ready:", ctx.Err())
		case <-tick.C:
		}
	}
	pids := shardProcessIDs(logOutput())
	if len(pids) != 2 || pids[0] == pids[1] {
		t.Fatalf("expected two distinct shard processes, got %v", pids)
	}
	for _, pid := range pids {
		if pid == cmd.Process.Pid {
			t.Fatalf("shard process has the root PID %d", pid)
		}
		if err := signalProcess(pid, syscall.Signal(0)); err != nil {
			t.Fatalf("shard process %d is unavailable: %v", pid, err)
		}
	}

	call := func(endpoint, method string, result any, args ...any) {
		t.Helper()
		client, err := rpc.DialContext(ctx, endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if err := client.CallContext(ctx, result, method, args...); err != nil {
			t.Fatalf("%s at %s: %v", method, endpoint, err)
		}
	}

	type block struct {
		Hash common.Hash
	}
	var root struct {
		block
		ShardBlockHeaders []block
	}
	call(endpoints[0], "master_getBlockByNumber", &root, "0x0")
	if root.Hash == (common.Hash{}) || len(root.ShardBlockHeaders) != 2 {
		t.Fatalf("expected R0 with both shard genesis headers, got %+v", root)
	}
	for i, endpoint := range endpoints[1:] {
		var chainID hexutil.Uint64
		call(endpoint, "eth_chainId", &chainID)
		if want := hexutil.Uint64(100001 + i); chainID != want {
			t.Fatalf("shard at %s has chain ID %d, want %d", endpoint, chainID, want)
		}
		var genesis block
		call(endpoint, "eth_getBlockByNumber", &genesis, "0x0", false)
		if genesis.Hash != root.ShardBlockHeaders[i].Hash {
			t.Fatalf("root did not confirm shard %d genesis %s", chainID, genesis.Hash)
		}
		// The shard rejects work until it has received its root checkpoint.
		var work struct{ SealHash common.Hash }
		call(endpoint, "qkc_getWork", &work, common.Address{19: byte(i + 1)})
		if work.SealHash == (common.Hash{}) {
			t.Fatalf("shard %d returned empty mining work", chainID)
		}
	}
	if stop == os.Kill {
		if err := signalProcess(pids[0], stop); err != nil {
			t.Fatal(err)
		}
		shardCrashed = true
	}
	return root.Hash
}

func shardProcessIDs(logs string) []int {
	var pids []int
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, "Started shard process") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(field, "pid="); ok {
				if pid, err := strconv.Atoi(strings.ReplaceAll(value, ",", "")); err == nil && pid > 0 {
					pids = append(pids, pid)
				}
			}
		}
	}
	return pids
}

func signalProcess(pid int, signal os.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Signal(signal)
}
