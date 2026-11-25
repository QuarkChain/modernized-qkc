// Copyright 2026 The quarkchain Authors
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

package mining

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestSimulatedSearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const blockTime = 10 * time.Second
		search := NewSimulatedSearch(blockTime)
		// Seals are never earlier than the candidate's timestamp.
		work := &MiningWork{Time: uint64(time.Now().Unix()) + 5}
		if _, _, ok := search.Search(context.Background(), work); !ok || time.Now().Before(time.Unix(int64(work.Time), 0)) {
			t.Fatal("sealed before the candidate's timestamp")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, ok := search.Search(ctx, &MiningWork{Time: work.Time + 3600}); ok {
			t.Fatal("cancelled search sealed")
		}
		// Delays average the block time.
		const samples = 500
		var total time.Duration
		for range samples {
			start := time.Now()
			search.Search(context.Background(), &MiningWork{Time: uint64(start.Unix())})
			total += time.Since(time.Unix(start.Unix(), 0))
		}
		if mean := total / samples; mean < blockTime*8/10 || mean > blockTime*12/10 {
			t.Fatalf("mean delay %v, want about %v", mean, blockTime)
		}
	})
}
