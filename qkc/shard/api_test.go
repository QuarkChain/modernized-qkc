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

package shard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

type miningRPCSubmission struct {
	nonce        types.BlockNonce
	hash, digest common.Hash
}

type miningRPCBackend struct {
	work        [4]string
	err         error
	accept      bool
	getCalls    int
	submissions []miningRPCSubmission
}

func (b *miningRPCBackend) GetWork() ([4]string, error) {
	b.getCalls++
	return b.work, b.err
}

func (b *miningRPCBackend) SubmitWork(nonce types.BlockNonce, hash, digest common.Hash) bool {
	b.submissions = append(b.submissions, miningRPCSubmission{nonce, hash, digest})
	return b.accept
}

// No concrete consensus engine is needed for RPC routing. Header verification
// is the only consensus operation used while initializing these test chains.
type miningRPCEngine struct {
	noVerifyEngine
	*miningRPCBackend
}

type miningRPCResponse struct {
	Version string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newMiningRPCServer(t *testing.T, apis []rpc.API) *rpc.Server {
	t.Helper()
	server := rpc.NewServer()
	t.Cleanup(server.Stop)
	for _, api := range apis {
		if err := server.RegisterName(api.Namespace, api.Service); err != nil {
			t.Fatal(err)
		}
	}
	return server
}

func callMiningRPC(t *testing.T, server *rpc.Server, method, params string) miningRPCResponse {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response miningRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Version != "2.0" || response.ID != 1 {
		t.Fatalf("unexpected RPC envelope: %s", recorder.Body.String())
	}
	return response
}

func TestMiningRPCRouting(t *testing.T) {
	backends := []*miningRPCBackend{
		{work: [4]string{common.Hash{0x11}.Hex(), common.Hash{0x22}.Hex(), common.Hash{0x33}.Hex(), "0x1"}, accept: true},
		{work: [4]string{common.Hash{0x44}.Hex(), common.Hash{0x55}.Hex(), common.Hash{0x66}.Hex(), "0x100"}, accept: true},
	}
	var servers []*rpc.Server
	for _, backend := range backends {
		engine := &miningRPCEngine{miningRPCBackend: backend}
		env := newTestEnvWithEngine(t, engine, nil)
		servers = append(servers, newMiningRPCServer(t, env.sc.APIs()))
	}
	for i, server := range servers {
		response := callMiningRPC(t, server, "eth_getWork", "[]")
		if response.Error != nil {
			t.Fatalf("shard %d: getWork error: %+v", i, response.Error)
		}
		var work []string
		if err := json.Unmarshal(response.Result, &work); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(work, backends[i].work[:]) {
			t.Fatalf("shard %d: work %v, want %v", i, work, backends[i].work)
		}
		params := fmt.Sprintf(`["0x0102030405060708",%q,%q]`, work[0], common.Hash{0x77}.Hex())
		response = callMiningRPC(t, server, "eth_submitWork", params)
		if response.Error != nil || string(response.Result) != "true" {
			t.Fatalf("shard %d: submitWork result %s, error %+v", i, response.Result, response.Error)
		}
		want := miningRPCSubmission{types.EncodeNonce(0x0102030405060708), common.HexToHash(work[0]), common.Hash{0x77}}
		if got := backends[i].submissions; len(got) != 1 || got[0] != want || backends[i].getCalls != 1 {
			t.Fatalf("shard %d: submissions %+v, want %+v; getWork calls %d", i, got, want, backends[i].getCalls)
		}
		other := backends[1-i]
		if i == 0 && (other.getCalls != 0 || len(other.submissions) != 0) {
			t.Fatal("request reached the other shard's backend")
		}
	}
	if backends[0].getCalls != 1 || len(backends[0].submissions) != 1 {
		t.Fatal("second shard's requests reached the first shard's backend")
	}
}

func TestMiningRPCUnavailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend *miningRPCBackend
		message string
	}{
		{"unsupported", nil, "mining not supported"},
		{"no work", &miningRPCBackend{err: errors.New("no mining work available yet")}, "no mining work available yet"},
		{"stopped", &miningRPCBackend{err: errors.New("mining stopped")}, "mining stopped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var apis []rpc.API
			if test.backend == nil {
				env := newTestEnvWithEngine(t, noVerifyEngine{}, nil)
				apis = env.sc.APIs()
			} else {
				// Even if a backend returns data alongside an error, it must not
				// appear as available work in the JSON-RPC response.
				test.backend.work = [4]string{"0x1", "0x2", "0x3", "0x4"}
				apis = []rpc.API{{Namespace: "eth", Service: NewMiningAPI(test.backend)}}
			}
			server := newMiningRPCServer(t, apis)
			response := callMiningRPC(t, server, "eth_getWork", "[]")
			if response.Error == nil || response.Error.Code != -32000 || response.Error.Message != test.message || len(response.Result) != 0 {
				t.Fatalf("getWork result %s, error %+v, want error %q", response.Result, response.Error, test.message)
			}
			params := fmt.Sprintf(`["0x0000000000000000",%q,%q]`, common.Hash{}.Hex(), common.Hash{}.Hex())
			response = callMiningRPC(t, server, "eth_submitWork", params)
			if response.Error != nil || string(response.Result) != "false" {
				t.Fatalf("submitWork result %s, error %+v, want false", response.Result, response.Error)
			}
		})
	}
}

func TestMiningRPCSubmissionResult(t *testing.T) {
	backend := &miningRPCBackend{}
	server := newMiningRPCServer(t, []rpc.API{{Namespace: "eth", Service: NewMiningAPI(backend)}})
	params := fmt.Sprintf(`["0x0000000000000000",%q,%q]`, common.Hash{}.Hex(), common.Hash{}.Hex())
	for _, accept := range []bool{false, true} {
		backend.accept = accept
		response := callMiningRPC(t, server, "eth_submitWork", params)
		if response.Error != nil || string(response.Result) != fmt.Sprint(accept) {
			t.Fatalf("submitWork result %s, error %+v, want %t", response.Result, response.Error, accept)
		}
	}
	if len(backend.submissions) != 2 || backend.submissions[0] != (miningRPCSubmission{}) || backend.submissions[1] != (miningRPCSubmission{}) {
		t.Fatalf("unexpected submissions: %+v", backend.submissions)
	}
}

func TestMiningRPCInvalidParams(t *testing.T) {
	backend := &miningRPCBackend{accept: true}
	server := newMiningRPCServer(t, []rpc.API{{Namespace: "eth", Service: NewMiningAPI(backend)}})
	hash := common.Hash{}.Hex()
	for _, test := range []struct {
		name, method, params string
	}{
		{"getWork extra argument", "eth_getWork", `["0x1"]`},
		{"missing arguments", "eth_submitWork", `[]`},
		{"missing digest", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000",%q]`, hash)},
		{"extra argument", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000",%q,%q,"0x1"]`, hash, hash)},
		{"short nonce", "eth_submitWork", fmt.Sprintf(`["0x1",%q,%q]`, hash, hash)},
		{"long nonce", "eth_submitWork", fmt.Sprintf(`["0x000000000000000000",%q,%q]`, hash, hash)},
		{"unprefixed nonce", "eth_submitWork", fmt.Sprintf(`["0000000000000000",%q,%q]`, hash, hash)},
		{"numeric nonce", "eth_submitWork", fmt.Sprintf(`[1,%q,%q]`, hash, hash)},
		{"null nonce", "eth_submitWork", fmt.Sprintf(`[null,%q,%q]`, hash, hash)},
		{"short hash", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000","0x11",%q]`, hash)},
		{"long hash", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000",%q,%q]`, hash+"00", hash)},
		{"short digest", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000",%q,"0x22"]`, hash)},
		{"invalid digest hex", "eth_submitWork", fmt.Sprintf(`["0x0000000000000000",%q,%q]`, hash, "0x"+strings.Repeat("gg", 32))},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := callMiningRPC(t, server, test.method, test.params)
			if response.Error == nil || response.Error.Code != -32602 || len(response.Result) != 0 {
				t.Fatalf("result %s, error %+v, want invalid params", response.Result, response.Error)
			}
		})
	}
	if backend.getCalls != 0 || len(backend.submissions) != 0 {
		t.Fatalf("invalid requests reached backend: getWork calls %d, submissions %+v", backend.getCalls, backend.submissions)
	}
}
