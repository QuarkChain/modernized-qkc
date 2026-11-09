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

package genesis

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/qkc/config"
	qkctypes "github.com/ethereum/go-ethereum/qkc/types"
)

func rootSeedFile(t *testing.T, input string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "root.json")
	if !strings.Contains(input, `"config":`) {
		cfg, err := json.Marshal(rootSeedConfig())
		if err != nil {
			t.Fatal(err)
		}
		input = strings.Replace(input, "{", `{"config":`+string(cfg)+",", 1)
	}
	if err := os.WriteFile(file, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func rootSeedConfig() config.RootConfig {
	return config.RootConfig{
		ConsensusType: params.ConsensusEthash,
		PoSW: &params.PoSWConfig{
			AdjustmentCutoff: 7, AdjustmentFactor: 512, MinimumDifficulty: big.NewInt(2),
			CoinbaseAmount: new(big.Int),
			EpochInterval:  10, DecayFactor: big.NewRat(88, 100),
		},
	}
}

func readRootForTest(file string) (*config.RootGenesis, error) {
	node := config.DefaultNodeConfig(nil)
	node.GenesisFile = file
	if err := node.LoadGenesis(); err != nil {
		return nil, err
	}
	return node.Root, nil
}

func loadRootForTest(file string, headers []*types.Header) (*qkctypes.RootBlock, error) {
	seed, err := readRootForTest(file)
	if err != nil {
		return nil, err
	}
	return buildRoot(seed, headers)
}

func rootSeedHeaders() []*types.Header {
	return []*types.Header{
		{Number: new(big.Int), Difficulty: new(big.Int), Extra: []byte{1}},
		{Number: new(big.Int), Difficulty: new(big.Int), Extra: []byte{2}},
	}
}

func TestBuildRoot(t *testing.T) {
	file := rootSeedFile(t, `{"timestamp":"0x2a","difficulty":"0xa","coinbase":"0x1111111111111111111111111111111111111111","extraData":"0x1234","nonce":"0x7","mixHash":"0x0000000000000000000000000000000000000000000000000000000000000009"}`)
	headers := rootSeedHeaders()
	seed, err := readRootForTest(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg := seed.RootConfig
	root, err := buildRoot(seed, headers)
	if err != nil {
		t.Fatal(err)
	}
	if root.Time() != 42 || root.Difficulty().Uint64() != 10 || root.TotalDifficulty().Uint64() != 10 || root.Nonce() != 7 {
		t.Fatal("root seed header fields were not preserved")
	}
	if root.Coinbase() != common.HexToAddress("0x1111111111111111111111111111111111111111") || string(root.Extra()) != "\x12\x34" || root.MixDigest() != common.HexToHash("0x9") {
		t.Fatal("optional root seed fields were not preserved")
	}
	if root.Version() != 0 || root.Number() != 0 || root.ParentHash() != (common.Hash{}) || !root.CoinbaseAmount().IsZero() || root.Signature() != ([65]byte{}) || len(root.TrackingData()) != 0 {
		t.Fatal("root genesis structural fields are invalid")
	}
	if root.ShardHeaderHash() != qkctypes.CalcShardHeaderHash(headers) {
		t.Fatal("root does not commit to the shard genesis headers")
	}
	again, err := buildRoot(seed, headers)
	if err != nil || again.Hash() != root.Hash() {
		t.Fatalf("same input did not produce the same root: %v", err)
	}
	reversed, err := buildRoot(seed, []*types.Header{headers[1], headers[0]})
	if err != nil || reversed.Hash() == root.Hash() {
		t.Fatalf("root failed to preserve network shard order: %v", err)
	}
	headers[0].Extra[0] = 3
	if root.ShardBlockHeaders()[0].Extra[0] != 1 || root.ShardBlockHeaders()[0].Hash() != again.ShardBlockHeaders()[0].Hash() {
		t.Fatal("root aliases its input shard header")
	}
	if cfg.PoSW.MinimumDifficulty.Int64() != 2 {
		t.Fatal("root generation changed its PoSW configuration")
	}
}

func TestBuildRootDefaultsAndUint256(t *testing.T) {
	file := rootSeedFile(t, `{"timestamp":0,"difficulty":2}`)
	root, err := loadRootForTest(file, rootSeedHeaders())
	if err != nil {
		t.Fatal(err)
	}
	if root.Time() != 0 || root.Coinbase() != (common.Address{}) || len(root.Extra()) != 0 || root.Nonce() != 0 || root.MixDigest() != (common.Hash{}) {
		t.Fatal("root optional fields do not default to zero")
	}
	maximum := "0x" + strings.Repeat("f", 64)
	file = rootSeedFile(t, `{"timestamp":"0xffffffffffffffff","difficulty":"`+maximum+`"}`)
	root, err = loadRootForTest(file, rootSeedHeaders())
	if err != nil {
		t.Fatal(err)
	}
	if root.Difficulty().ToBig().BitLen() != 256 || root.TotalDifficulty().Cmp(root.Difficulty()) != 0 || root.Time() != ^uint64(0) {
		t.Fatal("root difficulty or timestamp was truncated")
	}
}

func TestBuildRootRequiresHeaders(t *testing.T) {
	file := rootSeedFile(t, `{"timestamp":0,"difficulty":2}`)
	for _, headers := range [][]*types.Header{nil, {nil}} {
		if _, err := loadRootForTest(file, headers); err == nil {
			t.Fatal("missing shard header accepted")
		}
	}
}
