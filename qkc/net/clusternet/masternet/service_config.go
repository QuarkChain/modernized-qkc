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

package masternet

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/qkc/net/clusternet"
	"github.com/ethereum/go-ethereum/qkc/net/conn"
)

// slaveConnectRetryDelay matches Python master dial retry pacing.
const slaveConnectRetryDelay = time.Second

// startupNetworkTimeout sets each dial timeout and startup RPC context deadline.
// RPC contexts do not interrupt write-lock waits or network writes; the transport
// enforces a separate write timeout. Stop cancels startup contexts and closes
// connections to interrupt writes. Dial retries have no overall deadline.
const startupNetworkTimeout = 20 * time.Second

// ServiceConfig supplies deployment settings for one slave per configured chain.
type ServiceConfig struct {
	SlaveInfos         []clusternet.SlaveInfo
	ConfiguredChainIDs []*big.Int
	Logger             log.Logger
	// Shutdown requests asynchronous node shutdown after an unexpected slave
	// disconnect or initialization failure. It is called at most once, never
	// as a result of Stop. It must return promptly. The owner stops Service
	// before releasing master resources and stops the rest of the cluster.
	Shutdown func()
}

// validate checks deployment settings and applies defaults.
func (cfg *ServiceConfig) validate() error {
	if len(cfg.ConfiguredChainIDs) == 0 {
		return errors.New("configured chain IDs must not be empty")
	}
	if cfg.Shutdown == nil {
		return errors.New("missing node shutdown callback")
	}
	if len(cfg.ConfiguredChainIDs) != len(cfg.SlaveInfos) {
		return errors.New("configured chain IDs and slaves must have equal counts")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Root()
	}
	configured := make(map[string]struct{}, len(cfg.ConfiguredChainIDs))
	for _, id := range cfg.ConfiguredChainIDs {
		if err := conn.ValidateSlaveChainID(id); err != nil {
			return err
		}
		configured[id.String()] = struct{}{}
	}
	for i, info := range cfg.SlaveInfos {
		if info.Host == "" || info.Port == 0 {
			return fmt.Errorf("invalid slave address at index %d", i)
		}
		if err := conn.ValidateSlaveChainID(info.ChainID); err != nil {
			return err
		}
		key := info.ChainID.String()
		if _, ok := configured[key]; !ok {
			return fmt.Errorf("slave chain ID %s is unconfigured or duplicated", key)
		}
		delete(configured, key)
	}
	return nil
}
