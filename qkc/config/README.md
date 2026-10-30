# Network, deployment and process configuration

The shared network TOML file defines `NetworkID`, root-chain PoSW, and each
shard's native geth `params.ChainConfig`. `NetworkID` is the uint32 P2P network
identifier. Each shard has its own positive uint256 EVM `ChainID` for transaction
replay protection. They are independent identifiers; a network ID may equal a
chain ID. Shard IDs route traffic within the network and are separate from both.

Each shard is identified by `Shards.QuarkChain.ShardID`. Shard 0 is required for
root rewards and staking. Shard IDs and EVM chain IDs must be unique; shard ID
`4294967295` is reserved for root-chain traffic. An omitted
`Shards.QuarkChain.ShardIDs` defaults to all configured shard IDs in ascending
order. An explicit list must contain every shard exactly once.

`Root.PoSW` and each `Shards.QuarkChain.PoSW` require an independent
`params.PoSWConfig`. Each must specify positive `AdjustmentCutoff`,
`AdjustmentFactor`, `MinimumDifficulty`, and `EpochInterval`, a nonnegative
`CoinbaseAmount`, and `RewardTaxRate` and `DecayFactor` between zero and one.
Use quoted decimal or hex integer strings for `MinimumDifficulty`,
`CoinbaseAmount`, and `StakePerBlock`; these amounts must fit uint256. Use quoted
decimal strings for the ratios, such as `"0.5"` and `"0.88"`.

`StakingEnabled` defaults to false, selecting ordinary PoW without the staking
discount; the other consensus parameters are still required. Enabling staking
also requires positive `WindowSize`, `StakePerBlock`, and `DiffDivider`.
`OriginalBlockNumber` continues reward and staking decay from an old chain's
height, and `PoSWStakingDecayEnabled` explicitly enables stake requirement decay.
These settings do not control staking contract deployment; the deployment rules
from #18 still apply. The example uses zero block subsidies.

`cluster.toml` describes one Master process and one process per shard. It
references the shared network file through `NetworkFile`. `[Master.Node]`
reuses geth's `node.Config`. Each `[[ShardNodes]]` entry has a routing `ShardID`,
an internal TCP `Address`, its own `[ShardNodes.Node]`, and native
`ethconfig.Config` under `[ShardNodes.Eth]`. Master has no EVM execution config.
Every network shard must appear exactly once in the deployment, with a distinct
internal address and persistent data directory.

`--qkc.role master` selects Master. `--qkc.role shard --qkc.shard <id>`
selects one shard by routing ID, including zero; the selector is required and is
independent of the EVM chain ID. Master's effective shard endpoint map is derived
from the deployment entries. There is no second endpoint list to maintain.
`Address` is the shard's listening and advertised internal TCP endpoint, so it
requires a concrete host and a port from 1 to 65535. Connection management and
handshakes are handled separately by #16.

Master owns external devp2p through `Master.Node.P2P`. Shards disable geth's
external P2P by default and must keep it disabled; their internal TCP service
uses `Address`. HTTP/WS/IPC settings are independent for each process.
The example assigns different HTTP ports to the two shards and puts IPC sockets
under separate data directories.

Both TOML files follow geth conventions: case-sensitive Go field names, native
geth types, and errors for unknown fields and duplicate keys. Fork block numbers
and chain IDs use geth's quoted big-integer representation.

Process-local precedence is **explicit CLI values > selected deployment entry >
role defaults**. Overrides apply only to the selected process. Defaults reuse
`node.DefaultConfig` and, for shards, `ethconfig.Defaults`. Each process gets
its own default data directory under `qkc/master` or `qkc/shard-<id>`,
with `qkc.ipc`; shards add the `eth` RPC module. HTTP and WebSocket are disabled
by default. No Ethereum bootstrap nodes or network preset are injected.

Network rules come exclusively from the shared file. No network or EVM chain ID
is guessed. A shard's effective `Eth.NetworkId` comes from `NetworkID`; a
nonzero deployment `Eth.NetworkId` or explicit `--networkid` must match it.
`--networkid=0`, local `Eth.Genesis`, and local fork overrides are rejected.
Master rejects `Eth` configuration and shard execution flags.

Validate each process in the three-process example from the repository root:

```sh
go run ./cmd/geth checkqkcconfig --config qkc/config/examples/cluster.toml --qkc.role master
go run ./cmd/geth checkqkcconfig --config qkc/config/examples/cluster.toml --qkc.role shard --qkc.shard 0
go run ./cmd/geth checkqkcconfig --config qkc/config/examples/cluster.toml --qkc.role shard --qkc.shard 1 --http=false
```

`NetworkFile` is resolved relative to the deployment file. `--qkc.network`
replaces that path and is relative to the working directory. Other relative
paths retain geth's working-directory semantics. The check validates the entire
deployment after applying selected-process overrides, without creating data
directories or starting services. Genesis generation and database initialization
are subsequent PRs in #28; runtime service assembly belongs to #22.

`checkqkcconfig --help` lists supported overrides for data storage, sync mode,
P2P listening and peer limits, HTTP/WS/IPC, and RPC caps. Unset flags preserve
the file's values, including zero and false. Explicit `--http=false` or
`--ws=false` disables a configured endpoint. An address flag alone changes an
already enabled endpoint's address. Ethereum network presets and other
unsupported geth flags are rejected. HTTP and WS may share a port as in geth;
port zero requests a dynamically assigned port.

The package API is `LoadCluster`, optionally replace `ClusterConfig.NetworkFile`,
`SelectNode`, apply explicit process-local CLI values, `LoadNetwork`, then
`ClusterConfig.Validate`. Selection returns the chosen `NodeConfig` within the
deployment. Validation binds each shard's `Eth.NetworkId` to the network ID.
