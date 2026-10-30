# Network and node configuration

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

The local TOML file references the shared file through `NetworkFile` and embeds
geth's `node.Config` under `[Node]` and `ethconfig.Config` under `[Eth]`. These
execution settings are shared by the shards hosted in this single-node cluster.
Both files follow geth TOML conventions: case-sensitive Go field names, native
geth types, and errors for unknown fields and duplicate keys. Fork block numbers
and chain IDs use geth's quoted big-integer representation.

Local precedence is **explicit CLI values > local TOML > defaults**. Defaults
come from `node.DefaultConfig` and `ethconfig.Defaults`, with a `qkc` subdirectory
under geth's default data directory, `qkc.ipc`, and the `eth` RPC module added.
HTTP and WebSocket remain disabled by default. No Ethereum bootstrap nodes or
network preset are injected. No network or EVM chain ID is guessed. The effective
`Eth.NetworkId` always comes from `NetworkID`; a nonzero local `Eth.NetworkId`
or explicit `--networkid` must match. `--networkid=0` is rejected. Local
`Eth.Genesis` and fork overrides are rejected because they change network rules.

Inspect and validate the examples from the repository root:

```sh
go run ./cmd/geth checkqkcconfig --config qkc/config/examples/node.toml
go run ./cmd/geth checkqkcconfig --config qkc/config/examples/node.toml --http=false --maxpeers 0
go run ./cmd/geth checkqkcconfig --qkc.network qkc/config/examples/network.toml --datadir ./qkc-data
```

`NetworkFile` is resolved relative to the local file; `--qkc.network` is relative
to the working directory. Other relative paths retain geth's working-directory
semantics. The check does not create data directories or start services.
Genesis generation, database initialization, and cluster startup are subsequent
parts of issue #28.

`checkqkcconfig --help` lists supported overrides for data storage, sync mode,
P2P listening and peer limits, HTTP/WS/IPC, and RPC caps. Unset flags preserve
the file's values, including zero and false. Explicit `--http=false` or
`--ws=false` disables a configured endpoint. An address flag alone changes an
already enabled endpoint's address. Ethereum network presets and other
unsupported geth flags are rejected. HTTP and WS may share a port as in geth;
port zero requests a dynamically assigned port.

The package API is `LoadNode`, apply local CLI values, `LoadNetwork`, then
`NodeConfig.Validate`. The last step validates the combined settings and binds
`Eth.NetworkId` to the network ID.
