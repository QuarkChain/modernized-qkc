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

// Package clusternet_test verifies milestone 1: communication between one master and two slaves over real TCP.
//
// Topology: master <-> slave A (10001), master <-> slave B (10002), and A <-> B.
// All three services, both shards, transaction execution, and database codecs use real implementations.
// Databases are in memory, and simulated consensus avoids mining. With RootChain not yet integrated,
// the test builds root blocks R0/R1 and records reports in a stub master backend. Shard confirmation and storage are real.
// Services are assembled directly, independently of command-line startup, external P2P, or PeerConn.
//
//   - TestClusterCommunication verifies startup, cross-shard delivery, header reports, and root confirmation.
//   - TestClusterActiveShutdown verifies that stopping a receiver interrupts pending RPCs and stops its peers.
//   - TestClusterFailureShutdown verifies that a report handler failure closes the connection and stops all three nodes.
//
// Each test owner asynchronously stops its own service when notified of shutdown.
package clusternet_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/poswshard"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/clusternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/conn"
	"github.com/ethereum/go-ethereum/qkc/clusternet/masternet"
	"github.com/ethereum/go-ethereum/qkc/clusternet/slavenet"
	qkcrawdb "github.com/ethereum/go-ethereum/qkc/rawdb"
	"github.com/ethereum/go-ethereum/qkc/shard"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
	"github.com/holiman/uint256"
)

const (
	chainID0     int64 = 10001 // The first configured chain serves root-chain stake queries.
	chainID1     int64 = 10002
	depositValue       = 100
)

// TestClusterCommunication verifies the complete successful internal communication flow.
// A executes a cross-shard transaction, delivers its deposit to B, and reports the header only after acknowledgement.
// B delivers an empty list in reverse. The master queries both shards, broadcasts R1, and verifies their confirmation state.
func TestClusterCommunication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)

	// 1. After identity checks and slave interconnection, PING(R0) initializes both real shards.
	t.Log("1. Start the master and two slaves, establish three TCP connections, and initialize R0")
	cluster := startCluster(t, ctx, sender, nil)

	// 2. A executes an Outbox call, delivers the deposit to B, then reports its header. B delivers an empty list in reverse.
	t.Log("2. Execute a cross-shard transfer; verify A-to-B deposits, B-to-A empty lists, and reports after delivery")
	blocks := verifyCrossShardDelivery(t, ctx, cluster, key)

	// 3. Query shard headers, build and broadcast R1, then verify stored payloads and confirmations.
	t.Log("3. Query headers, broadcast R1, verify both shards' confirmations, and query root-chain stakes")
	verifyRootConfirmation(t, ctx, cluster, blocks)

	assertNoShutdownNotification(t, cluster.shutdown)
}

// TestClusterActiveShutdown stops a receiver while it is handling a request, verifies that the pending RPC
// returns a connection-closed error, and waits for the other two nodes to stop after notification.
func TestClusterActiveShutdown(t *testing.T) {
	for _, initiator := range []string{"master", "slave"} {
		t.Run(initiator, func(t *testing.T) {
			// 1. Start the cluster and initialize R0. A hook pauses the request after it reaches the receiver.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			releaseTask := sync.OnceFunc(func() { close(release) })
			defer releaseTask()
			handle := func() error {
				close(entered)
				<-release
				return nil
			}
			hooks := &clusterHooks{report: handle}
			if initiator == "slave" {
				hooks = &clusterHooks{deposits: handle}
			}
			cluster := startCluster(t, ctx, common.Address{}, hooks)
			a, b := cluster.shards[0], cluster.shards[1]

			// 2. Import a shard block and wait for the handler. A's block pauses the master's report handler;
			// B's block pauses A's cross-shard handler. The sender is still waiting for its RPC response.
			sender := a
			owner := cluster.masterOwner
			wantShutdown := []string{a.owner.name, b.owner.name}
			if initiator == "slave" {
				sender = b
				owner = a.owner
				wantShutdown = []string{cluster.masterOwner.name, b.owner.name}
			}
			block := buildShardBlock(t, ctx, sender)
			result := make(chan error, 1)
			go func() {
				defer close(result)
				result <- sender.backend.AddShardBlock(ctx, block)
			}()
			// Wait for the import to return before releasing shard resources, even if an assertion fails.
			t.Cleanup(func() { <-result })
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("request returned before reaching the backend: %v", err)
			case <-ctx.Done():
				t.Fatalf("waiting for request handler: %v", ctx.Err())
			}

			// 3. Stop only the receiver. The caller must observe disconnection before the handler is released.
			assertNoShutdownNotification(t, cluster.shutdown)
			t.Logf("Stop %s while %s is still waiting for a response", owner.name, sender.owner.name)
			owner.stopAsync()
			select {
			case err := <-result:
				if !errors.Is(err, conn.ErrConnectionClosed) {
					t.Fatalf("pending request after %s disconnected: %v", owner.name, err)
				}
			case <-ctx.Done():
				t.Fatalf("pending request did not return after %s disconnected: %v", owner.name, ctx.Err())
			}

			// 4. Verify both peers receive shutdown notifications, release the handler, then wait for all three services to stop.
			waitForShutdownNotifications(t, ctx, cluster.shutdown, wantShutdown...)
			releaseTask()
			waitForClusterStop(t, ctx, cluster)
			assertNoShutdownNotification(t, cluster.shutdown)
		})
	}
}

// TestClusterFailureShutdown verifies that a report handler failure closes the connection and notifies all three nodes to stop.
func TestClusterFailureShutdown(t *testing.T) {
	// 1. Start the cluster and initialize R0. Make the master fail while handling a shard header report.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	cluster := startCluster(t, ctx, common.Address{}, &clusterHooks{
		report: func() error {
			close(entered)
			return errors.New("could not store shard header")
		},
	})
	a, b := cluster.shards[0], cluster.shards[1]
	assertNoShutdownNotification(t, cluster.shutdown)

	// 2. Import a block on A and verify that its report reaches the master's failure hook.
	block := buildShardBlock(t, ctx, a)
	err := a.backend.AddShardBlock(ctx, block)
	select {
	case <-entered:
	default:
		t.Fatalf("report never reached the failing handler: %v", err)
	}

	// 3. The handler failure must close the reporting connection, returning ErrConnectionClosed to A.
	if !errors.Is(err, conn.ErrConnectionClosed) {
		t.Fatalf("report error: got %v, want closed connection", err)
	}

	// 4. Wait for all three notifications and service stops. Callbacks initiate shutdown; the test only observes it.
	waitForShutdownNotifications(t, ctx, cluster.shutdown, cluster.masterOwner.name, a.owner.name, b.owner.name)
	waitForClusterStop(t, ctx, cluster)
	assertNoShutdownNotification(t, cluster.shutdown)
}

// waitForShutdownNotifications collects the expected nodes' notifications in any order.
func waitForShutdownNotifications(t *testing.T, ctx context.Context, notifications <-chan string, services ...string) {
	t.Helper()
	pending := make(map[string]bool, len(services))
	for _, service := range services {
		pending[service] = true
	}
	for len(pending) > 0 {
		select {
		case service := <-notifications:
			if !pending[service] {
				t.Fatalf("unexpected shutdown notification from %s, waiting for %v", service, pending)
			}
			t.Logf("%s received a shutdown notification; its owner asynchronously stops the service", service)
			delete(pending, service)
		case <-ctx.Done():
			t.Fatalf("waiting for shutdown notifications from %v: %v", pending, ctx.Err())
		}
	}
}

// waitForClusterStop waits for Stop to return on all three services.
func waitForClusterStop(t *testing.T, ctx context.Context, cluster *testCluster) {
	t.Helper()
	owners := []*clusterServiceOwner{cluster.masterOwner, cluster.shards[0].owner, cluster.shards[1].owner}
	for _, owner := range owners {
		if err := owner.waitForStop(ctx); err != nil {
			t.Fatalf("stop %s: %v", owner.name, err)
		}
	}
}

func assertNoShutdownNotification(t *testing.T, notifications <-chan string) {
	t.Helper()
	select {
	case service := <-notifications:
		t.Fatalf("unexpected shutdown notification from %s", service)
	default:
	}
}

// startCluster assembles the nodes directly: listen, connect, verify identities, interconnect, then PING(R0).
// Hooks affect only the master's report handler or A's receive handler. The normal path uses two complete real shards.
func startCluster(t *testing.T, ctx context.Context, sender common.Address, hooks *clusterHooks) *testCluster {
	t.Helper()
	if hooks == nil {
		hooks = new(clusterHooks)
	}

	chainIDs := []int64{chainID0, chainID1}
	configured := make([]*big.Int, len(chainIDs))
	for i, id := range chainIDs {
		configured[i] = big.NewInt(id)
	}
	// Each service notifies at most once. Capacity for all three nodes keeps normal shutdown callbacks from blocking.
	shutdown := make(chan string, len(chainIDs)+1)
	masterOwner := newClusterServiceOwner("master")
	masterConfig := masternet.ServiceConfig{
		ConfiguredChainIDs: configured,
		Shutdown: func() {
			shutdown <- "master"
			masterOwner.stopAsync()
		},
	}
	cluster := &testCluster{shutdown: shutdown, masterOwner: masterOwner}

	// Start both slave listeners on ephemeral ports before giving their addresses to the master.
	var genesisHeaders qkctypes.ShardBlockHeaders
	for _, id := range configured {
		owner := newClusterServiceOwner(fmt.Sprintf("slave %s", id))
		service, err := slavenet.NewService(slavenet.ServiceConfig{
			ListenAddr:         "127.0.0.1:0",
			LocalChainID:       id,
			ConfiguredChainIDs: configured,
			Shutdown: func() {
				shutdown <- owner.name
				owner.stopAsync()
			},
		})
		if err != nil {
			t.Fatalf("create slave %s: %v", id, err)
		}

		node := newTestShard(t, id.Int64(), chainIDs, sender, service)
		node.owner = owner
		owner.stop = service.Stop
		cluster.shards = append(cluster.shards, node)
		genesisHeaders = append(genesisHeaders, node.chain.Genesis().Header())
		// Cleanup runs in reverse registration order: stop communication and drain handlers before releasing shard resources.
		owner.registerCleanup(t)
		var backend slavenet.ShardBackend = node.backend
		if id.Int64() == chainID0 && hooks.deposits != nil {
			backend = &clusterShardBackend{Shard: node.backend, deposits: hooks.deposits}
		}
		if err := service.Start(backend); err != nil {
			t.Fatalf("start slave %s: %v", id, err)
		}

		addr := service.Addr().(*net.TCPAddr)
		masterConfig.SlaveInfos = append(masterConfig.SlaveInfos, clusternet.SlaveInfo{
			ChainID: id,
			Host:    addr.IP.String(),
			Port:    uint16(addr.Port),
		})
	}

	// Build R0 with both shards' real genesis headers in place of the not-yet-integrated RootChain.
	cluster.rootGenesis = qkctypes.NewRootBlock(&qkctypes.RootBlockHeader{
		Difficulty:      *uint256.NewInt(1),
		TotalDifficulty: *uint256.NewInt(1),
	}, genesisHeaders, nil)

	a, b := cluster.shards[0], cluster.shards[1]
	cluster.masterBackend = &clusterMasterBackend{
		destinationDBs: map[string]ethdb.Database{
			a.chainID().String(): b.db,
			b.chainID().String(): a.db,
		},
		reports: make(chan shardHeaderReport, 1),
		report:  hooks.report,
	}

	master, err := masternet.NewService(masterConfig)
	if err != nil {
		t.Fatalf("create master: %v", err)
	}
	masterOwner.stop = master.Stop
	masterOwner.registerCleanup(t)
	cluster.master = master
	if err := master.Start(cluster.masterBackend); err != nil {
		t.Fatalf("start master: %v", err)
	}

	// WaitForInitialization covers identity checks and slave interconnection, not business initialization.
	select {
	case <-master.WaitForInitialization():
		if !master.IsActive() {
			t.Fatal("master did not activate after initialization")
		}
	case service := <-shutdown:
		t.Fatalf("%s requested shutdown during startup", service)
	case <-ctx.Done():
		t.Fatalf("initialize cluster: %v", ctx.Err())
	}

	if err := master.Ping(ctx, cluster.rootGenesis); err != nil {
		t.Fatalf("initialize shards: %v", err)
	}

	for _, node := range cluster.shards {
		if got, want := node.backend.RootTip(), cluster.rootGenesis.Hash(); got != want {
			t.Fatalf("shard %s root tip after initialization: got %s, want %s", node.chainID(), got, want)
		}
	}
	return cluster
}

// verifyCrossShardDelivery imports real blocks in both directions and checks the resulting cross-shard state.
// A delivers one deposit to B; B has no outgoing transfer but must still deliver an empty list to A.
// The returned blocks are later confirmed by the root block broadcast.
func verifyCrossShardDelivery(t *testing.T, ctx context.Context, cluster *testCluster, key *ecdsa.PrivateKey) [2]*types.Block {
	t.Helper()

	a, b := cluster.shards[0], cluster.shards[1]
	recipient := common.Address{19: 0xb1}

	// 1. Execute A's Outbox transaction, build a block, and import it. The shard sends the deposit to B
	// and waits for storage and acknowledgement before reporting. The master backend checks this order when the report arrives.
	submitOutboxTransfer(t, a, b.chainID(), key, recipient, depositValue)
	blockA := importShardBlock(t, ctx, cluster.masterBackend, a)

	// 2. Extract deposits from A's actual receipts and compare their full contents with B's stored data.
	byChainID, err := shard.XShardDeposits(a.chain, blockA.Hash())
	if err != nil {
		t.Fatalf("read deposits from shard %s receipts: %v", a.chainID(), err)
	}
	sent := byChainID[b.chainID().String()]
	if len(sent) != 1 {
		t.Fatalf("block %s generated %d deposits for shard %s, want 1", blockA.Hash(), len(sent), b.chainID())
	}
	stored := qkcrawdb.ReadXShardDeposits(b.db, blockA.Hash())
	if len(stored) != 1 {
		t.Fatalf("shard %s stored %d deposits from %s, want 1", b.chainID(), len(stored), blockA.Hash())
	}
	if types.NewTx(stored[0]).Hash() != types.NewTx(sent[0]).Hash() {
		t.Fatalf("shard %s stored deposit %+v, want %+v", b.chainID(), stored[0], sent[0])
	}
	t.Log("A -> B: the stored deposit matches A's execution receipt")

	// 3. B must deliver an empty list even without outgoing transfers. An empty list must differ from missing data,
	// so A can check data availability when a root block confirming B's block arrives.
	blockB := importShardBlock(t, ctx, cluster.masterBackend, b)
	reverse := qkcrawdb.ReadXShardDeposits(a.db, blockB.Hash())
	if reverse == nil || len(reverse) != 0 {
		t.Fatalf("shard %s did not store an empty deposit list from %s: %v", a.chainID(), blockB.Hash(), reverse)
	}
	t.Log("B -> A: an empty deposit list is stored and distinguishable from missing data")
	return [2]*types.Block{blockA, blockB}
}

// submitOutboxTransfer signs a real Outbox call and submits it to the source shard's transaction pool.
// It only prepares the transaction; the real shard builds and executes the block and delivers the deposit.
func submitOutboxTransfer(t *testing.T, source *testShard, destination *big.Int, key *ecdsa.PrivateKey, recipient common.Address, value int64) {
	t.Helper()

	outbox, err := abi.JSON(strings.NewReader(`[
        {"type":"function","name":"send","stateMutability":"payable","inputs":[
            {"name":"dstShard","type":"uint256"},
            {"name":"to","type":"address"},
            {"name":"value","type":"uint256"},
            {"name":"gasLimit","type":"uint64"},
            {"name":"isMove","type":"bool"},
            {"name":"data","type":"bytes"}
        ]}
    ]`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := outbox.Pack("send", destination, recipient, big.NewInt(value), params.XShardSendGas, true, []byte{})
	if err != nil {
		t.Fatalf("pack Outbox call: %v", err)
	}

	gasPrice := big.NewInt(2 * params.GWei)
	// The call value must cover both the transfer amount and prepaid execution gas on the destination shard.
	prepaid := new(big.Int).Mul(new(big.Int).SetUint64(params.XShardSendGas), gasPrice)
	prepaid.Add(prepaid, big.NewInt(value))
	tx := types.MustSignNewTx(key, types.LatestSigner(source.chain.Config()), &types.LegacyTx{
		To:       &params.XShardOutboxAddress,
		Value:    prepaid,
		Data:     data,
		Gas:      200_000,
		GasPrice: gasPrice,
	})
	if err := source.pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
		t.Fatalf("submit Outbox transaction: %v", err)
	}
}

// importShardBlock builds and imports a real shard block, waiting for cross-shard delivery and the header report.
// It checks the reported chain ID, block hash, and coinbase amount rather than relying on local import success.
func importShardBlock(t *testing.T, ctx context.Context, master *clusterMasterBackend, node *testShard) *types.Block {
	t.Helper()

	block := buildShardBlock(t, ctx, node)
	if err := node.backend.AddShardBlock(ctx, block); err != nil {
		t.Fatalf("import and propagate block on shard %s: %v", node.chainID(), err)
	}
	amount, err := node.chain.GetCoinbaseAmount(block.Hash())
	if err != nil {
		t.Fatalf("read coinbase amount on shard %s: %v", node.chainID(), err)
	}

	// The master backend records the report before acknowledging it, so it must already be queued when propagation returns.
	select {
	case report := <-master.reports:
		if report.chainID.Cmp(node.chainID()) != 0 {
			t.Fatalf("reported chain ID: got %s, want %s", report.chainID, node.chainID())
		}
		if report.blockHash != block.Hash() {
			t.Fatalf("shard %s reported block %s, want %s", node.chainID(), report.blockHash, block.Hash())
		}
		if !report.amount.Eq(amount) {
			t.Fatalf("shard %s reported coinbase amount %s, want %s", node.chainID(), &report.amount, amount)
		}
	default:
		t.Fatalf("shard %s finished propagation without reporting to the master backend", node.chainID())
	}
	return block
}

// buildShardBlock builds a candidate that simulated consensus can import without mining.
func buildShardBlock(t *testing.T, ctx context.Context, node *testShard) *types.Block {
	t.Helper()
	block, _, err := node.backend.CreateBlockToMine(ctx, common.BigToAddress(node.chainID()))
	if err != nil {
		t.Fatalf("build block on shard %s: %v", node.chainID(), err)
	}
	return block
}

// verifyRootConfirmation checks header queries, root block broadcasts, and root-chain stake queries.
// R1 uses headers queried over the network. After broadcast, both shards must store it and update their confirmations.
func verifyRootConfirmation(t *testing.T, ctx context.Context, cluster *testCluster, blocks [2]*types.Block) {
	t.Helper()

	headers, err := cluster.master.GetUnconfirmedHeaderList(ctx)
	if err != nil {
		t.Fatalf("query unconfirmed headers: %v", err)
	}
	if len(headers) != len(cluster.shards) {
		t.Fatalf("header query returned %d chains, want %d", len(headers), len(cluster.shards))
	}
	var confirmed qkctypes.ShardBlockHeaders
	for i, node := range cluster.shards {
		list := headers[node.chainID().String()]
		if len(list) != 1 {
			t.Fatalf("shard %s returned %d unconfirmed headers, want 1", node.chainID(), len(list))
		}
		if got, want := list[0].Hash(), blocks[i].Hash(); got != want {
			t.Fatalf("shard %s returned header %s, want %s", node.chainID(), got, want)
		}
		confirmed = append(confirmed, list[0])
	}

	// Build R1 from headers queried over the network, without reading them directly from the local shards.
	timestamp := max(blocks[0].Time(), blocks[1].Time())
	next := cluster.rootGenesis.Header().CreateBlockToAppend(&timestamp, nil, nil, nil, nil)
	root := qkctypes.NewRootBlock(next.Header(), confirmed, nil)
	rootHash := root.Hash()
	encodedRoot, err := root.MarshalSSZ()
	if err != nil {
		t.Fatalf("encode R1: %v", err)
	}
	if err := cluster.master.BroadcastRootBlock(ctx, root); err != nil {
		t.Fatalf("broadcast R1: %v", err)
	}

	// Root consensus and full body commitment validation belong to the future RootChain. Here, both real shards
	// must store the complete R1 payload, advance their root tips, and confirm their respective shard blocks.
	for i, node := range cluster.shards {
		if got := node.backend.RootTip(); got != rootHash {
			t.Fatalf("shard %s root tip after broadcast: got %s, want %s", node.chainID(), got, rootHash)
		}
		if !bytes.Equal(qkcrawdb.ReadRootBlockSSZ(node.db, rootHash), encodedRoot) {
			t.Fatalf("shard %s stored a different R1 payload for %s", node.chainID(), rootHash)
		}
		if got, want := qkcrawdb.ReadLastConfirmedBlockHash(node.db, rootHash), blocks[i].Hash(); got != want {
			t.Fatalf("shard %s last confirmed block: got %s, want %s", node.chainID(), got, want)
		}
		t.Logf("%s stored R1 and updated its local shard confirmation", node.owner.name)
	}

	// Query an unstaked address on the first configured chain; both the stake amount and signer must be zero.
	stakes, err := cluster.master.GetRootChainStakes(ctx, cluster.shards[0].chainID(), common.Address{}, blocks[0].Hash())
	if err != nil {
		t.Fatalf("query root chain stakes: %v", err)
	}
	if !stakes.Stakes.IsZero() || stakes.Signer != (common.Address{}) {
		t.Fatalf("unexpected root stakes for an unstaked address: %+v", stakes)
	}
}

type testCluster struct {
	master        *masternet.Service
	masterOwner   *clusterServiceOwner
	masterBackend *clusterMasterBackend
	rootGenesis   *qkctypes.RootBlock
	shards        []*testShard // Configuration order: index 0 is A, index 1 is B.
	shutdown      <-chan string
}

// clusterServiceOwner stands in for process assembly, asynchronously stopping its service and recording completion.
// A shutdown callback stops only its local service. Other nodes must detect TCP closure and stop themselves.
// Closing stopped means Service.Stop has returned and business resources can be released.
type clusterServiceOwner struct {
	name    string
	stop    func() error
	once    sync.Once
	stopped chan struct{}
	err     error
}

func newClusterServiceOwner(name string) *clusterServiceOwner {
	return &clusterServiceOwner{name: name, stopped: make(chan struct{})}
}

// stopAsync starts shutdown once, asynchronously, so request handlers and shutdown callbacks can call it safely.
// Service.Stop drains request handlers; waiting here could deadlock the caller with the handler being drained.
func (o *clusterServiceOwner) stopAsync() {
	o.once.Do(func() {
		go func() {
			o.err = o.stop()
			close(o.stopped)
		}()
	})
}

func (o *clusterServiceOwner) waitForStop(ctx context.Context) error {
	select {
	case <-o.stopped:
		return o.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// registerCleanup waits for the communication service to stop before releasing shard resources.
func (o *clusterServiceOwner) registerCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		o.stopAsync()
		if err := o.waitForStop(context.Background()); err != nil {
			t.Errorf("cleanup %s: %v", o.name, err)
		}
	})
}

// clusterHooks controls shutdown tests; deposits applies only to shard A.
type clusterHooks struct {
	report   func() error
	deposits func() error
}

// clusterShardBackend runs a test hook before storing a cross-shard list.
type clusterShardBackend struct {
	*shard.Shard
	deposits func() error
}

func (b *clusterShardBackend) AddXshardTxList(hash common.Hash, deposits []*types.DepositTx) error {
	if err := b.deposits(); err != nil {
		return err
	}
	return b.Shard.AddXshardTxList(hash, deposits)
}

type testShard struct {
	db      ethdb.Database
	chain   *core.BlockChain
	pool    *txpool.TxPool
	backend *shard.Shard
	owner   *clusterServiceOwner
}

// BlockChain and TxPool expose the existing chain and pool to the block builder through miner.Backend.
func (s *testShard) BlockChain() *core.BlockChain { return s.chain }
func (s *testShard) TxPool() *txpool.TxPool       { return s.pool }

func (s *testShard) chainID() *big.Int { return s.chain.Config().ChainID }

// newTestShard assembles a full shard with test genesis, real system contracts, an in-memory database, chain, pool, and builder.
// The sender receives funds for contract calls; client supplies the real slave communication service.
// Simulated consensus skips mining searches; validation, execution, cross-shard delivery, and storage remain real.
func newTestShard(t *testing.T, id int64, chainIDs []int64, sender common.Address, client shard.ClusterClient) *testShard {
	t.Helper()

	config := params.NewTestQuarkChainConfig(id, chainIDs...)
	config.QuarkChain.ConsensusType = params.ConsensusSimulate
	alloc := core.SystemContractAllocs()
	maps.Copy(alloc, core.PoSWStakingContractAllocs(config))
	alloc[sender] = types.Account{Balance: big.NewInt(params.Ether)}
	genesis := &core.Genesis{
		Config:     config,
		GasLimit:   params.GenesisGasLimit,
		Difficulty: new(big.Int),
		Mixhash:    common.BigToHash(common.Big1),
		ExtraData: (&types.QKCExtra{
			ChainID:        config.ChainID,
			Cursor:         types.XShardCursor{RootBlockHeight: 1}, // Begin consuming cross-shard deposits after root genesis.
			XShardGasLimit: params.GenesisGasLimit / params.XShardGasLimitDivisor,
		}).Encode(),
		Alloc: alloc,
	}

	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() { db.Close() })
	engine, err := poswshard.NewSimulate(config, genesis.ToBlock().Header())
	if err != nil {
		t.Fatalf("create shard %d consensus engine: %v", id, err)
	}
	t.Cleanup(func() { engine.Close() })
	chain, err := core.NewBlockChain(db, genesis, engine, nil)
	if err != nil {
		t.Fatalf("create shard %d blockchain: %v", id, err)
	}
	t.Cleanup(chain.Stop)

	poolConfig := legacypool.DefaultConfig
	poolConfig.Journal = "" // Do not create a transaction pool journal on disk.
	pool, err := txpool.New(poolConfig.PriceLimit, chain, []txpool.SubPool{legacypool.New(poolConfig, chain)})
	if err != nil {
		t.Fatalf("create shard %d transaction pool: %v", id, err)
	}
	t.Cleanup(func() { pool.Close() })

	node := &testShard{db: db, chain: chain, pool: pool}
	minerConfig := miner.DefaultConfig
	minerConfig.GasCeil = genesis.GasLimit
	builder := miner.New(node, minerConfig, engine)
	node.backend, err = shard.New(shard.NewShardChain(db, chain, builder), client)
	if err != nil {
		t.Fatalf("create shard %d backend: %v", id, err)
	}
	return node
}

// clusterMasterBackend is a stub master backend that receives shard header reports in place of RootChain.
// It checks that the destination stored the cross-shard list before recording the report, verifying delivery-before-report ordering.
// Shutdown tests check delivery ordering before the report hook blocks the request or injects a failure.
type clusterMasterBackend struct {
	// Map the reporting chain to its deposit receiver's database: A reports against B, and B against A.
	destinationDBs map[string]ethdb.Database
	reports        chan shardHeaderReport
	report         func() error // Optional fault hook for connection lifecycle tests.
}

// shardHeaderReport records what the master received for comparison with the source shard's actual block.
type shardHeaderReport struct {
	chainID   *big.Int
	blockHash common.Hash
	amount    uint256.Int
}

// AddShardBlockHeader is called by the real master service when a report arrives, never directly by the test.
// The service acknowledges only after success, making this the observation point for delivery-before-report ordering.
func (b *clusterMasterBackend) AddShardBlockHeader(chainID *big.Int, header *types.Header, amount *uint256.Int) error {
	blockHash := header.Hash()
	destination := b.destinationDBs[chainID.String()]
	if destination == nil {
		return fmt.Errorf("unexpected header report from shard %s", chainID)
	}
	// Check the destination database when the report arrives to catch reports sent before deposit delivery.
	if !qkcrawdb.HasXShardDeposits(destination, blockHash) {
		return fmt.Errorf("shard %s reported block %s before its deposits were stored", chainID, blockHash)
	}
	if b.report != nil {
		return b.report()
	}
	select {
	case b.reports <- shardHeaderReport{chainID: chainID, blockHash: blockHash, amount: *amount}:
		return nil
	default:
		return fmt.Errorf("unexpected extra header report from shard %s", chainID)
	}
}
