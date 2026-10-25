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

package posw

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

func TestDifficultyReference(t *testing.T) {
	config := &params.PoWConfig{AdjustmentCutoff: 10, AdjustmentFactor: 2048, MinimumDifficulty: uint256.NewInt(1)}
	for _, test := range []struct{ delta, want uint64 }{{0, 2049000}, {9, 2049000}, {10, 2048000}, {20, 2047000}, {1000, 1949000}, {math.MaxUint64, 1949000}} {
		parent := big.NewInt(2048000)
		if got := CalcDifficulty(config, test.delta, 0, parent); !got.IsUint64() || got.Uint64() != test.want {
			t.Fatalf("delta %d: %v, want %d", test.delta, got, test.want)
		}
		if parent.Int64() != 2048000 {
			t.Fatal("modified parent")
		}
	}
	config.MinimumDifficulty.SetUint64(2000000)
	if got := CalcDifficulty(config, 1000, 0, big.NewInt(2048000)); got.Int64() != 2000000 {
		t.Fatal(got)
	}
}

func TestAdjustedDifficulty(t *testing.T) {
	c := &params.PoSWConfig{Enabled: true, WindowSize: 3, StakePerBlock: uint256.NewInt(100), DiffDivider: 100}
	base := big.NewInt(100)
	for _, test := range []struct {
		stakes *uint256.Int
		mined  uint64
		want   int64
	}{
		{uint256.NewInt(99), 0, 100}, {uint256.NewInt(100), 0, 1}, {uint256.NewInt(100), 1, 100},
		{uint256.NewInt(300), 2, 1}, {new(uint256.Int).SetAllOne(), 2, 1},
	} {
		if got := GetAdjustedDifficulty(c, base, test.stakes, test.mined); got.Int64() != test.want {
			t.Fatalf("stakes %s, mined %d: %v", test.stakes, test.mined, got)
		}
	}
	if base.Int64() != 100 {
		t.Fatal("modified base difficulty")
	}
	c.Enabled = false
	if got := GetAdjustedDifficulty(c, base, uint256.NewInt(100), 0); got.Int64() != 100 {
		t.Fatal(got)
	}
}
