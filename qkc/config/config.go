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

// Package config loads QuarkChain root and shard TOML settings.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/node"
	"github.com/naoina/toml"
)

// TOML uses geth's case-sensitive Go field names and rejects unknown fields.
var tomlSettings = toml.Config{
	NormFieldName: func(_ reflect.Type, key string) string { return key },
	FieldToKey:    func(_ reflect.Type, field string) string { return field },
}

func load(file string, cfg interface{}) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tomlSettings.NewDecoder(f).Decode(cfg); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func checkRootNode(cfg *node.Config) error {
	if cfg.PrivateHost != "" || cfg.PrivatePort != 0 {
		return errors.New("only shards can configure PrivateHost or PrivatePort")
	}
	if cfg.DBEngine != "" && cfg.DBEngine != "pebble" && cfg.DBEngine != "leveldb" {
		return fmt.Errorf("invalid Node.DBEngine %q", cfg.DBEngine)
	}
	for name, value := range map[string]int{
		"Node.P2P.MaxPeers": cfg.P2P.MaxPeers, "Node.P2P.MaxPendingPeers": cfg.P2P.MaxPendingPeers,
	} {
		if value < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	for name, port := range map[string]int{"Node.HTTPPort": cfg.HTTPPort, "Node.WSPort": cfg.WSPort, "Node.AuthPort": cfg.AuthPort} {
		if port < 0 || port > math.MaxUint16 {
			return fmt.Errorf("%s must be between 0 and 65535", name)
		}
	}
	for name, addr := range map[string]string{"Node.P2P.ListenAddr": cfg.P2P.ListenAddr, "Node.P2P.DiscAddr": cfg.P2P.DiscAddr} {
		if addr == "" {
			continue
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("%s: invalid port %q", name, port)
		}
	}
	// macOS has a 104-byte Unix socket path, including the terminator.
	if endpoint := cfg.IPCEndpoint(); runtime.GOOS != "windows" && len(endpoint) > 103 {
		return fmt.Errorf("IPC endpoint exceeds Unix socket path limit: %s", endpoint)
	}
	return nil
}

// canonicalHost normalizes IP literals and hostname case without resolving DNS.
func canonicalHost(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(host)
}

func checkPrivateEndpoint(host string, port int) error {
	ip, err := netip.ParseAddr(host)
	if host == "" || strings.ContainsAny(host, " \t\r\n/[]") ||
		(strings.Contains(host, ":") && err != nil) || (err == nil && ip.Unmap().IsUnspecified()) {
		return fmt.Errorf("PrivateHost must be a concrete host without brackets or a port: %q", host)
	}
	if port < 1 || port > math.MaxUint16 {
		return errors.New("PrivatePort must be between 1 and 65535")
	}
	return nil
}
