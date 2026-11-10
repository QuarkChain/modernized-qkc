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

// Command sszgenfix bounds generated dynamic-list allocations by input size.
// fastssz's DecodeDynamicLength derives the element count from the first offset
// without checking whether the offset table fits in the input. Generated
// decoders allocate the list before UnmarshalDynamic rejects a malformed table,
// so a short payload with a large offset can trigger a huge allocation. Run
// this after sszgen to cap the count by the bytes actually available.
package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
)

func main() {
	const path = "messages_ssz.go"
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	call := regexp.MustCompile(`ssz\.DecodeDynamicLength\(buf, ([^()]+)\)`)
	updated := call.ReplaceAll(data, []byte("ssz.DecodeDynamicLength(buf, min($1, len(buf)/4))"))
	if bytes.Equal(data, updated) {
		panic(fmt.Errorf("no dynamic-list decoders found in %s", path))
	}
	if err := os.WriteFile(path, updated, 0644); err != nil {
		panic(err)
	}
}
