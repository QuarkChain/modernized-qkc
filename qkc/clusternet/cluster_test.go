// Copyright 2026-2027 The quarkchain Authors
// This file is part of the quarkchain library.
//
// The quarkchain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The quarkchain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the quarkchain library. If not, see <http://www.gnu.org/licenses/>.

// These tests connect one master and two slaves over real TCP using mock backends.
// Shutdown callback counts distinguish passive failures from active Stop calls.
package clusternet_test

import (
	"context"
	"errors"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/conn"
	"github.com/ethereum/go-ethereum/qkc/clusternet/masternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/slavenet"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

const (
	slaveAChainID int64 = 10001
	slaveBChainID int64 = 10002
)

// TestMockClusterCommunication exercises all three links, then stops the master.
// Only the two slaves should request Shutdown after losing their connections.
func TestMockClusterCommunication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cluster := newMockCluster()

	// Set up the payloads and receiver checks before opening the TCP links.
	checkpoint := qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{Number: 1})
	checkpointHash := checkpoint.Hash()
	shardBlockHash := common.Hash{1}
	header := &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}
	headerHash, amount := header.Hash(), uint256.NewInt(7)
	sourceChainID := big.NewInt(slaveAChainID)

	var pingsA, pingsB, reports, depositsA, depositsB atomic.Int32
	checkPing := func(node string, calls *atomic.Int32) func(*qkctypes.RootBlock) error {
		return func(root *qkctypes.RootBlock) error {
			if got := root.Hash(); got != checkpointHash {
				t.Errorf("%s checkpoint hash: got %v, want %v", node, got, checkpointHash)
			}
			calls.Add(1)
			return nil
		}
	}
	cluster.slaveA.onInit = checkPing("slave A", &pingsA)
	cluster.slaveB.onInit = checkPing("slave B", &pingsB)
	cluster.master.onHeader = func(chainID *big.Int, header *types.Header, coinbaseAmount *uint256.Int) error {
		if hash := header.Hash(); chainID.Cmp(sourceChainID) != 0 || hash != headerHash || *coinbaseAmount != *amount {
			t.Errorf("header report (chain, hash, amount): got (%v, %v, %v), want (%v, %v, %v)",
				chainID, hash, coinbaseAmount, sourceChainID, headerHash, amount)
		}
		reports.Add(1)
		return nil
	}
	cluster.slaveA.onXshard = func(common.Hash, []*types.DepositTx) error {
		depositsA.Add(1)
		return nil
	}
	cluster.slaveB.onXshard = func(hash common.Hash, deposits []*types.DepositTx) error {
		if hash != shardBlockHash || len(deposits) != 0 {
			t.Errorf("cross-shard request (hash, deposit count): got (%v, %d), want (%v, 0)", hash, len(deposits), shardBlockHash)
		}
		depositsB.Add(1)
		return nil
	}
	cluster.start(t, ctx)

	// Step 1. The master sends a checkpoint PING to both slaves and waits for their PONGs.
	if err := cluster.master.service.Ping(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}

	// Step 2. Slave A sends an empty cross-shard deposit list to slave B and waits for its ACK.
	if err := cluster.slaveA.service.BroadcastXShardTxList(ctx, big.NewInt(slaveBChainID), shardBlockHash, nil); err != nil {
		t.Fatal(err)
	}

	// Step 3. Slave A reports a shard header to the master and waits for its ACK.
	if err := cluster.slaveA.service.SendShardBlockHeaderToMaster(ctx, header, amount); err != nil {
		t.Fatal(err)
	}

	// Step 4. Stop the master after the replies arrive. Only the two slaves should request Shutdown.
	go func() { cluster.master.stopped <- cluster.master.service.Stop() }()
	cluster.checkShutdownCalls(t, ctx, [3]int32{0, 1, 1})

	// Step 5. Once all handlers have drained, verify delivery counts, including that only B received the deposit list.
	if got, want := [5]int32{pingsA.Load(), pingsB.Load(), reports.Load(), depositsA.Load(), depositsB.Load()}, [5]int32{1, 1, 1, 0, 1}; got != want {
		t.Fatalf("handler calls [PING A, PING B, header report, cross-shard A, cross-shard B]: got %v, want %v", got, want)
	}
}

// TestMockClusterActiveShutdown stops a receiver while its handler is blocked.
// The pending RPC must fail before the handler returns; only peers call Shutdown.
func TestMockClusterActiveShutdown(t *testing.T) {
	t.Run("master", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)

		cluster := newMockCluster()
		cluster.master.onHeader = func(*big.Int, *types.Header, *uint256.Int) error {
			close(entered)
			<-release
			return nil
		}
		cluster.start(t, ctx)

		// Step 1. Slave A reports a header to the master. Wait until the master's handler is blocked.
		result := make(chan error, 1)
		header := &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}
		go func() {
			result <- cluster.slaveA.service.SendShardBlockHeaderToMaster(ctx, header, uint256.NewInt(1))
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("request did not reach the master handler:", ctx.Err())
		}

		// Step 2. Stop the master while the report is in flight. Slave A should see the connection close.
		go func() { cluster.master.stopped <- cluster.master.service.Stop() }()
		select {
		case err := <-result:
			if !errors.Is(err, conn.ErrConnectionClosed) {
				t.Fatalf("pending header request: got %v, want closed connection", err)
			}
		case <-ctx.Done():
			t.Fatal("pending header request did not return:", ctx.Err())
		}

		// Step 3. Check that Stop is still waiting, then release the handler. Only A and B should request Shutdown.
		select {
		case err := <-cluster.master.stopped:
			t.Fatalf("master Stop returned while the handler was blocked: %v", err)
		default:
		}
		release <- struct{}{}
		cluster.checkShutdownCalls(t, ctx, [3]int32{0, 1, 1})
	})

	t.Run("slave A", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		cluster := newMockCluster()
		cluster.slaveA.onInit = func(*qkctypes.RootBlock) error {
			close(entered)
			<-release
			return nil
		}
		cluster.start(t, ctx)

		// Step 1. The master sends a checkpoint PING to both slaves. Wait until A's handler is blocked.
		result := make(chan error, 1)
		checkpoint := qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{Number: 1})
		go func() { result <- cluster.master.service.Ping(ctx, checkpoint) }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("request did not reach slave A's handler:", ctx.Err())
		}

		// Step 2. Stop slave A while its PING is in flight. The master should see a connection close.
		go func() { cluster.slaveA.stopped <- cluster.slaveA.service.Stop() }()
		select {
		case err := <-result:
			if !errors.Is(err, conn.ErrConnectionClosed) {
				t.Fatalf("pending PING: got %v, want closed connection", err)
			}
		case <-ctx.Done():
			t.Fatal("pending PING did not return:", ctx.Err())
		}

		// Step 3. Check that Stop is still waiting, then release the handler.
		// Only the master and B should request Shutdown.
		select {
		case err := <-cluster.slaveA.stopped:
			t.Fatalf("slave A Stop returned while the handler was blocked: %v", err)
		default:
		}
		release <- struct{}{}
		cluster.checkShutdownCalls(t, ctx, [3]int32{1, 0, 1})
	})
}

// TestMockClusterFailureShutdown returns an error from either receiver's handler.
// All three nodes must request Shutdown exactly once without an explicit Stop.
func TestMockClusterFailureShutdown(t *testing.T) {
	t.Run("master", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		entered := make(chan struct{})
		cluster := newMockCluster()
		cluster.master.onHeader = func(*big.Int, *types.Header, *uint256.Int) error {
			close(entered)
			return errors.New("simulated header handler failure")
		}
		cluster.start(t, ctx)

		// Step 1. Slave A reports a header. The master's handler error should close A's connection to the master.
		header := &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}
		err := cluster.slaveA.service.SendShardBlockHeaderToMaster(ctx, header, uint256.NewInt(1))
		if !errors.Is(err, conn.ErrConnectionClosed) {
			t.Fatalf("failed header request: got %v, want closed connection", err)
		}

		// Step 2. Confirm that the report reached the handler that injects the failure.
		select {
		case <-entered:
		default:
			t.Fatal("header request disconnected before reaching the failing handler")
		}

		// Step 3. Verify that each node requests Shutdown exactly once and all three stop.
		cluster.checkShutdownCalls(t, ctx, [3]int32{1, 1, 1})
	})

	t.Run("slave A", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		entered := make(chan struct{})
		cluster := newMockCluster()
		cluster.slaveA.onInit = func(*qkctypes.RootBlock) error {
			close(entered)
			return errors.New("simulated PING handler failure")
		}
		cluster.start(t, ctx)

		// Step 1. The master sends a checkpoint PING. Slave A's handler error should close its master connection.
		checkpoint := qkctypes.NewRootBlockWithHeader(&qkctypes.RootBlockHeader{Number: 1})
		if err := cluster.master.service.Ping(ctx, checkpoint); !errors.Is(err, conn.ErrConnectionClosed) {
			t.Fatalf("failed PING: got %v, want closed connection", err)
		}

		// Step 2. Confirm that the PING reached slave A's failing handler.
		select {
		case <-entered:
		default:
			t.Fatal("PING disconnected before reaching slave A's failing handler")
		}

		// Step 3. Verify that each node requests Shutdown exactly once and all three stop.
		cluster.checkShutdownCalls(t, ctx, [3]int32{1, 1, 1})
	})
}

type mockCluster struct {
	master *mockMasterBackend
	slaveA *mockSlaveBackend
	slaveB *mockSlaveBackend
}

func newMockCluster() *mockCluster {
	return &mockCluster{
		master: &mockMasterBackend{stopped: make(chan error, 1)},
		slaveA: &mockSlaveBackend{stopped: make(chan error, 1)},
		slaveB: &mockSlaveBackend{stopped: make(chan error, 1)},
	}
}

// start connects all three nodes and completes identity checks and slave interconnection.
// Tests configure their handlers before starting the services.
func (cluster *mockCluster) start(t *testing.T, ctx context.Context) {
	t.Helper()
	master := cluster.master
	chainIDs := []*big.Int{big.NewInt(slaveAChainID), big.NewInt(slaveBChainID)}
	masterConfig := masternet.ServiceConfig{ConfiguredChainIDs: chainIDs, Shutdown: master.Shutdown}

	// Start both listeners on ephemeral ports before the master dials them.
	for index, slave := range []*mockSlaveBackend{cluster.slaveA, cluster.slaveB} {
		service, err := slavenet.NewService(slavenet.ServiceConfig{
			ListenAddr:         "127.0.0.1:0",
			LocalChainID:       chainIDs[index],
			ConfiguredChainIDs: chainIDs,
			Shutdown:           slave.Shutdown,
		})
		if err != nil {
			t.Fatal(err)
		}
		slave.service = service
		t.Cleanup(func() { service.Stop() })
		if err := service.Start(slave); err != nil {
			t.Fatal(err)
		}
		addr := service.Addr().(*net.TCPAddr)
		masterConfig.SlaveInfos = append(masterConfig.SlaveInfos, clusternet.SlaveInfo{
			ChainID: chainIDs[index],
			Host:    addr.IP.String(),
			Port:    uint16(addr.Port),
		})
	}

	service, err := masternet.NewService(masterConfig)
	if err != nil {
		t.Fatal(err)
	}
	master.service = service
	t.Cleanup(func() { service.Stop() })
	if err := service.Start(master); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.WaitForInitialization():
		if !service.IsActive() {
			t.Fatal("master did not activate after initialization")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// checkShutdownCalls waits for Stop to finish, then checks counts in master/A/B order.
// The stopped channels only synchronize completion; Shutdown is the counted event.
func (cluster *mockCluster) checkShutdownCalls(t *testing.T, ctx context.Context, want [3]int32) {
	t.Helper()
	for index, stopped := range []<-chan error{cluster.master.stopped, cluster.slaveA.stopped, cluster.slaveB.stopped} {
		select {
		case err := <-stopped:
			if err != nil {
				t.Fatalf("stop node %d: %v", index, err)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for node %d to stop: %v", index, ctx.Err())
		}
	}
	got := [3]int32{cluster.master.shutdownCalls.Load(), cluster.slaveA.shutdownCalls.Load(), cluster.slaveB.shutdownCalls.Load()}
	if got != want {
		t.Fatalf("Shutdown calls [master, slave A, slave B]: got %v, want %v", got, want)
	}
}

// mockMasterBackend accepts header requests without storing or validating blocks.
type mockMasterBackend struct {
	service       *masternet.Service
	shutdownCalls atomic.Int32
	stopped       chan error
	onHeader      func(*big.Int, *types.Header, *uint256.Int) error
}

// Shutdown records the service callback and schedules Stop outside the handler.
func (backend *mockMasterBackend) Shutdown() {
	backend.shutdownCalls.Add(1)
	go func() { backend.stopped <- backend.service.Stop() }()
}

func (backend *mockMasterBackend) AddShardBlockHeader(chainID *big.Int, header *types.Header, amount *uint256.Int) error {
	if backend.onHeader != nil {
		return backend.onHeader(chainID, header, amount)
	}
	return nil
}

// mockSlaveBackend accepts PING and cross-shard requests without creating a shard.
type mockSlaveBackend struct {
	service       *slavenet.Service
	shutdownCalls atomic.Int32
	stopped       chan error
	onInit        func(*qkctypes.RootBlock) error
	onXshard      func(common.Hash, []*types.DepositTx) error
}

// Shutdown uses the same asynchronous stop path as the master's mock backend.
func (backend *mockSlaveBackend) Shutdown() {
	backend.shutdownCalls.Add(1)
	go func() { backend.stopped <- backend.service.Stop() }()
}

func (backend *mockSlaveBackend) Init(root *qkctypes.RootBlock) error {
	if backend.onInit != nil {
		return backend.onInit(root)
	}
	return nil
}

// Unused backend operations fail if a test unexpectedly invokes them.
func (backend *mockSlaveBackend) AddRootBlock(*qkctypes.RootBlock) (bool, error) {
	return false, errors.New("unexpected AddRootBlock")
}

func (backend *mockSlaveBackend) GetRootChainStakes(common.Address, common.Hash) (*uint256.Int, common.Address, error) {
	return nil, common.Address{}, errors.New("unexpected GetRootChainStakes")
}

func (backend *mockSlaveBackend) GetUnconfirmedHeaderList() ([]*types.Header, error) {
	return nil, errors.New("unexpected GetUnconfirmedHeaderList")
}

func (backend *mockSlaveBackend) AddXshardTxList(hash common.Hash, deposits []*types.DepositTx) error {
	if backend.onXshard != nil {
		return backend.onXshard(hash, deposits)
	}
	return nil
}
