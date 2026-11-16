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

package eth

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posw"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/shard"
	"github.com/ethereum/go-ethereum/rpc"
)

// Use a fixed clock to reproduce the work and its full-size Ethash seal vectors.
// The RPC client knows only the served work; the node verifies the seal during
// import using the real PoSW engine, including its difficulty target.
func TestQKCMinerRPC(t *testing.T) {
	core.SenderCacher()
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The P2P node database's asynchronous LevelDB pool drain can
				// outlive Close by one second. Let it finish on the fake clock.
				t.Cleanup(func() {
					synctest.Wait()
					time.Sleep(time.Second)
					synctest.Wait()
				})
				config := qkcBackendConfig()
				config.StateScheme = scheme
				config.Genesis.Config.QuarkChain.ConsensusType = params.ConsensusEthash
				config.Genesis.Config.QuarkChain.PoSW.MinimumDifficulty = big.NewInt(2)
				config.Genesis.Mixhash = common.BigToHash(big.NewInt(2))
				// Keep the chain in memory: Pebble pools workers across databases,
				// which cannot be shared between separate synctest bubbles.
				stack, err := node.New(&node.Config{
					Name: "qkc-miner-test", HTTPHost: "127.0.0.1", HTTPModules: []string{"eth", "miner"},
					P2P: p2p.Config{NoDiscovery: true, NoDial: true},
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { stack.Close() })
				backend := startQKCBackend(t, stack, &config)
				if engine, ok := backend.Engine().(*posw.PoSW); !ok || engine.IsSimulate() {
					t.Fatal("mining RPC must use real Ethash verification")
				}
				t.Logf("Started POW_ETHASH node: scheme=%s rpc=%s", scheme, stack.HTTPEndpoint())
				client, err := rpc.DialHTTP(stack.HTTPEndpoint())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				coinbase := common.Address{0xaa}
				var work shard.MiningWork
				if err := client.Call(&work, "miner_getWork", coinbase); err == nil || !strings.Contains(err.Error(), shard.ErrRootChainUninitialized.Error()) {
					t.Fatalf("work before R0 initialization: %v", err)
				}
				initQKCBackend(t, backend)
				t.Logf("GetWork rejected before initialization; initialized R0=%s", backend.Shard().RootTip().TerminalString())
				if err := client.Call(&work, "miner_getWork", coinbase); err != nil {
					t.Fatal(err)
				}
				t.Logf("Received work: number=%d sealHash=%s difficulty=%s timestamp=%d", work.Number, work.SealHash.TerminalString(), work.Difficulty, work.Time)
				sealHash := common.HexToHash("0x114b8254cd8786627fc377b4c561b99819c20f0346a6f0108f1eb290003430ba")
				if work.SealHash != sealHash || work.Number != 1 || work.Time != 946684800 || work.Difficulty.Cmp(big.NewInt(2)) != 0 {
					t.Fatalf("unexpected work vector: %+v", work)
				}
				// Generated with full epoch-0 hashimotoLight from consensus/posw:
				// nonce 1's result starts 0x7297 (< 2^255); nonce 2's starts
				// 0x831b (> 2^255), so only nonce 1 meets difficulty 2.
				nonce := types.EncodeNonce(1)
				mixHash := common.HexToHash("0xed065151a317b2f3e3e6378474954a9aa6fb37c58b0e1887ebe73c9eb29850e0")
				for _, test := range []struct {
					name    string
					nonce   types.BlockNonce
					mixHash common.Hash
					want    string
				}{
					{"wrong mix", nonce, common.Hash{}, "invalid Ethash mix hash"},
					{"insufficient work", types.EncodeNonce(2), common.HexToHash("0x46a5b4261e58fcdbe91389ab08a03b50e369f34c296ac73f1be5179b4159f672"), "invalid Ethash proof of work"},
				} {
					var accepted bool
					err := client.Call(&accepted, "miner_submitWork", sealHash, test.nonce, test.mixHash)
					if err == nil || !strings.Contains(err.Error(), test.want) || accepted {
						t.Fatalf("%s: accepted=%t err=%v", test.name, accepted, err)
					}
					if head := backend.BlockChain().CurrentBlock(); head.Hash() != backend.BlockChain().Genesis().Hash() {
						t.Fatalf("%s advanced the head", test.name)
					}
					t.Logf("Rejected %s: nonce=%d error=%v; head remains at genesis", test.name, test.nonce.Uint64(), err)
				}
				var retry shard.MiningWork
				if err := client.Call(&retry, "miner_getWork", coinbase); err != nil || retry.SealHash != sealHash {
					t.Fatalf("invalid submission discarded work: %+v, %v", retry, err)
				}
				t.Logf("Resubmitting preserved work with valid seal: sealHash=%s nonce=%d mixHash=%s", retry.SealHash.TerminalString(), nonce.Uint64(), mixHash.TerminalString())
				var accepted bool
				if err := client.Call(&accepted, "miner_submitWork", sealHash, nonce, mixHash); err != nil || !accepted {
					t.Fatalf("valid seal rejected: accepted=%t err=%v", accepted, err)
				}
				var head *types.Header
				if err := client.Call(&head, "eth_getBlockByNumber", "latest", false); err != nil || head == nil {
					t.Fatalf("read imported block: %v", err)
				}
				extra, err := head.QKCExtra()
				if err != nil {
					t.Fatal(err)
				}
				importedSealHash, err := head.QKCSealHash()
				if err != nil || importedSealHash != sealHash || head.Number.Uint64() != 1 || head.Coinbase != coinbase || extra.Nonce != nonce || extra.MixHash != mixHash {
					t.Fatalf("head does not match the sealed work: %+v, %v", head, err)
				}
				if !backend.BlockChain().HasBlockAndState(head.Hash(), 1) {
					t.Fatal("sealed block and state were not imported")
				}
				t.Logf("Imported sealed block and state: number=%s hash=%s coinbase=%s", head.Number, head.Hash().TerminalString(), head.Coinbase)
				if err := client.Call(&accepted, "miner_submitWork", sealHash, nonce, mixHash); err == nil || !strings.Contains(err.Error(), shard.ErrUnknownWork.Error()) {
					t.Fatalf("imported work accepted again: %v", err)
				}
				t.Logf("Rejected replay of imported work: sealHash=%s", sealHash.TerminalString())
				if err := client.Call(&work, "miner_getWork", coinbase); err != nil || work.Number != 2 || work.SealHash == sealHash {
					t.Fatalf("work did not advance: %+v, %v", work, err)
				}
				t.Logf("Received next work: number=%d sealHash=%s difficulty=%s", work.Number, work.SealHash.TerminalString(), work.Difficulty)
			})
		})
	}
}

func TestMinerRPCRequiresQKC(t *testing.T) {
	api := NewMinerAPI(&Ethereum{})
	if _, err := api.GetWork(context.Background(), common.Address{}); err == nil {
		t.Fatal("non-QKC node served mining work")
	}
	if ok, err := api.SubmitWork(common.Hash{}, types.BlockNonce{}, common.Hash{}); err == nil || ok {
		t.Fatal("non-QKC node accepted mining work")
	}
}
