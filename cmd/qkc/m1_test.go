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
	"bytes"
	"context"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/qkc/comm"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestM1Root(t *testing.T) {
	const fixture = "../../qkc/config/devnet/root.toml"
	root, err := loadM1Root(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if root.Hash() != common.HexToHash("0xccd7e6a1ec8231f53a6f00399d9daf5e76529a52050b13f8187dbbf597718052") {
		t.Fatal("devnet root hash changed")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		from string
		to   string
	}{
		{"hash", root.Hash().Hex(), (common.Hash{}).Hex()},
		{"body commitment", root.ShardHeaderHash().Hex(), (common.Hash{}).Hex()},
		{"body", root.ShardBlockHeaders()[0].Root.Hex(), (common.Hash{}).Hex()},
		{"empty header", "[[ShardBlockHeaders]]", "[[ShardBlockHeaders]]\n\n[[ShardBlockHeaders]]"},
		{"missing number", `Number = "0"`, `# Number = "0"`},
		{"unknown field", "Hash =", "Unknown ="},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := bytes.Replace(data, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(mutated, data) {
				t.Fatal("fixture mutation did not apply")
			}
			path := filepath.Join(t.TempDir(), "root.toml")
			if err := os.WriteFile(path, mutated, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadM1Root(path); err == nil {
				t.Fatal("accepted a corrupt root fixture")
			}
		})
	}
}

func TestM1SlaveConfig(t *testing.T) {
	for i, id := range []int{100001, 100002} {
		path := fmt.Sprintf("../../qkc/config/devnet/shard-%d.toml", id)
		info, err := loadM1Slave(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.ChainID.Int64() != int64(id) || info.Host != "127.0.0.1" || info.Port != uint16(38000+i) {
			t.Fatalf("incorrect devnet cluster endpoint: %+v", info)
		}
		if info.rpcEndpoint != fmt.Sprintf("qkc-data/shard-%d/qkc-shard-%d.ipc", id, id) {
			t.Fatalf("IPC endpoint did not follow the TOML: %s", info.rpcEndpoint)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// IPC can be disabled and the private port changed without extra flags.
		data = bytes.Replace(data, []byte(fmt.Sprintf("PrivatePort = %d", info.Port)), []byte("PrivatePort = 39000"), 1)
		data = bytes.Replace(data, []byte(fmt.Sprintf("IPCPath = \"qkc-shard-%d.ipc\"", id)), []byte("IPCPath = \"\""), 1)
		path = filepath.Join(t.TempDir(), "shard.toml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		info, err = loadM1Slave(path)
		if err != nil || info.Port != 39000 {
			t.Fatalf("cluster endpoint did not follow the TOML: %+v, %v", info, err)
		}
		if info.rpcEndpoint != fmt.Sprintf("http://127.0.0.1:%d", 18545+i) {
			t.Fatalf("HTTP fallback did not follow the TOML: %s", info.rpcEndpoint)
		}
	}
}

type m1HeadAPI struct {
	id     *hexutil.Big
	height hexutil.Uint64
}

func (a m1HeadAPI) ChainId() *hexutil.Big       { return a.id }
func (a m1HeadAPI) BlockNumber() hexutil.Uint64 { return a.height }

// Checking the live RPC head prevents the offline fixture from rewinding an
// existing chain, while ordinary geth startup remains independent of the demo.
func TestM1CheckGenesis(t *testing.T) {
	for _, test := range []struct {
		name   string
		chain  int64
		height uint64
		want   string
	}{
		{"genesis", 100001, 0, ""},
		{"existing blocks", 100001, 7, "genesis-only"},
		{"wrong RPC node", 100002, 0, "does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := rpc.NewServer()
			defer server.Stop()
			if err := server.RegisterName("eth", m1HeadAPI{(*hexutil.Big)(big.NewInt(test.chain)), hexutil.Uint64(test.height)}); err != nil {
				t.Fatal(err)
			}
			http := httptest.NewServer(server)
			defer http.Close()
			slave := m1Slave{SlaveInfo: comm.SlaveInfo{ChainID: big.NewInt(100001)}, rpcEndpoint: http.URL}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := slave.checkGenesis(ctx)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func TestM1ReportedHeaders(t *testing.T) {
	root, err := loadM1Root("../../qkc/config/devnet/root.toml")
	if err != nil {
		t.Fatal(err)
	}
	header := types.CopyHeader(root.ShardBlockHeaders()[0])
	header.Number = big.NewInt(7)
	extra, err := header.QKCExtra()
	if err != nil {
		t.Fatal(err)
	}
	extra.PrevRootBlock = root.Hash()
	header.Extra = extra.Encode()
	master := &m1Master{root: root.Hash(), heights: map[string]uint64{"100001": 0}}
	if err := master.AddShardBlockHeader(extra.ChainID, header, nil); err != nil {
		t.Fatal(err)
	}
	if master.heights["100001"] != 7 {
		t.Fatal("header report did not update the displayed height")
	}
	if err := master.AddShardBlockHeader(big.NewInt(100002), header, nil); err == nil {
		t.Fatal("accepted a header from the wrong shard connection")
	}
	extra.PrevRootBlock = common.Hash{}
	header.Extra = extra.Encode()
	header.Number = big.NewInt(8)
	if err := master.AddShardBlockHeader(extra.ChainID, header, nil); err == nil {
		t.Fatal("accepted a header referencing another root")
	}
	if master.heights["100001"] != 7 {
		t.Fatal("rejected header changed the displayed height")
	}
}
