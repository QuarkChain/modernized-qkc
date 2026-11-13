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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
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
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Mining coordination only needs cluster TCP, not IPC or HTTP.
		data = bytes.Replace(data, []byte(fmt.Sprintf("PrivatePort = %d", info.Port)), []byte("PrivatePort = 39000"), 1)
		data = bytes.Replace(data, []byte(fmt.Sprintf("IPCPath = \"qkc-shard-%d.ipc\"", id)), []byte("IPCPath = \"\""), 1)
		data = bytes.Replace(data, []byte("HTTPHost = \"127.0.0.1\""), []byte("HTTPHost = \"\""), 1)
		path = filepath.Join(t.TempDir(), "shard.toml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		info, err = loadM1Slave(path)
		if err != nil || info.Port != 39000 {
			t.Fatalf("cluster endpoint did not follow the TOML: %+v, %v", info, err)
		}
	}
}
