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
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestMS1Root(t *testing.T) {
	const fixture = "../../qkc/config/devnet/root.toml"
	root, err := loadMS1Root(fixture)
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
			if _, err := loadMS1Root(path); err == nil {
				t.Fatal("accepted a corrupt root fixture")
			}
		})
	}
}
