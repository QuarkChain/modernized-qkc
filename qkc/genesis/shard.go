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
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/triedb"
)

// buildShard commits a prepared native genesis specification in memory.
func buildShard(spec *core.Genesis) (*Shard, error) {
	// Commit validates the allocation and returns errors while constructing the
	// native trie/block. Both databases are temporary and never touch datadirs.
	db := rawdb.NewMemoryDatabase()
	defer db.Close()
	tdb := triedb.NewDatabase(db, nil)
	defer tdb.Close()
	block, err := spec.Commit(db, tdb, nil)
	if err != nil {
		return nil, err
	}
	if err := block.Header().SanityCheckQKC(); err != nil {
		return nil, err
	}
	return &Shard{Genesis: spec, Block: block}, nil
}
