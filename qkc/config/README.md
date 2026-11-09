# QKC configuration

A QKC deployment uses one deployment TOML and a Genesis JSON for each chain:

| File | What it configures | Example |
| --- | --- | --- |
| `cluster.toml` | P2P network ID and process settings: one Master and one Slave per shard, with their Genesis files, addresses, data directories, and RPC settings. | [cluster.toml](examples/cluster.toml) |
| Root Genesis JSON | Root consensus settings under `config`, alongside root genesis header fields. | [root.json](examples/genesis/root.json) |
| Shard Genesis JSON | A complete native `core.Genesis`, including its `config` and allocation. | [shard-100001.json](examples/genesis/shard-100001.json), [shard-100002.json](examples/genesis/shard-100002.json) |

Each process selects its own entry from the deployment. `GenesisFile` points to
its chain's Genesis JSON. CLI flags override process settings; consensus rules
come from Genesis and the chain configuration stored in the database. There is
no separate network TOML or shared-fork expansion.

## Validate the example deployment

The example defines one Master and two Slaves. From the repository root:

```sh
# Master
go run ./cmd/qkc checkconfig \
  --config qkc/config/examples/cluster.toml --qkc.role master

# Chain 100001
go run ./cmd/qkc checkconfig \
  --config qkc/config/examples/cluster.toml --qkc.role slave --qkc.chain 100001

# Chain 100002, with its configured HTTP endpoint disabled
go run ./cmd/qkc checkconfig \
  --config qkc/config/examples/cluster.toml --qkc.role slave --qkc.chain 100002 --http=false
```

Each command selects a process and applies its overrides, explicitly loads all
referenced Genesis files, then validates the **entire deployment**. It checks consensus
configuration and cross-shard consistency, but does not construct genesis blocks,
validate the root header or allocation state, compare an existing database,
create data directories, or start services. Genesis initialization and node
startup remain separate operations.

To build the standalone CLI, run `make qkc`. Replace `go run ./cmd/qkc` with
`build/bin/qkc` in the commands above. Run `checkconfig --help` for supported flags.

## Configure consensus in Genesis JSON

Each shard file carries its full native [core.Genesis](../../core/genesis.go).
Its `config` is a [params.ChainConfig](../../params/config.go), with QKC settings
under `config.quarkchain`. Loading a shard's Genesis preserves this configuration
in `Eth.Genesis`; it does not overwrite it from another file. Chain initialization
uses the native Genesis and stored-config compatibility checks.

The root chain is not an EVM chain. Its Genesis `config` contains its own
`consensusType` and `posw`, independent of the shard settings. `checkconfig` reads
this root configuration; root block construction is outside its scope.

### Network ID, chain IDs, and shard order

| Field | Purpose | Requirement |
| --- | --- | --- |
| Deployment `NetworkID` | Identifies the P2P network. | Positive `uint64` (up to `2^64 - 1`), independent of EVM chain IDs. |
| Shard Genesis `config.chainId` | Identifies a shard for EVM replay protection and internal routing. | Unique positive `uint256`; matches the selected Slave's `ChainID`. |
| Shard Genesis `config.quarkchain.chainIds` | Ordered network membership. | Identical in every shard Genesis; contains every deployed shard exactly once. |

Chain IDs range from `1` to `2^256 - 1`; zero is reserved for root-chain traffic.
`Slaves.ChainID`, `--qkc.chain`, and the corresponding Genesis `config.chainId`
identify the same chain. The example uses `100001` and `100002`, with NetworkID `2`.

The first entry in `chainIds` receives root rewards and hosts the root staking
contract. Its order is part of the consensus configuration and must remain
unchanged for an initialized chain. The example uses `[100001, 100002]` in both
Genesis files. Reordering `[[Slaves]]` in the deployment does not change this
order. The loader never derives membership from the deployment entries.

### Forks

Specify fork heights directly in every shard's Genesis `config`. The following
heights must agree across shards: `homesteadBlock`, `eip150Block`, `eip155Block`,
`eip158Block`, `byzantiumBlock`, `constantinopleBlock`, `petersburgBlock`,
`istanbulBlock`, `berlinBlock`, and `londonBlock`. Nonzero heights are allowed;
omitted fields remain unset, while numeric `0` activates a fork at genesis.

Other native ChainConfig fields can be decoded, but their QKC runtime support
must be verified separately. Genesis files are complete inputs: there is no
inheritance or automatic insertion of missing forks or membership lists.

### PoSW and staking

Configure `config.posw` for root and `config.quarkchain.posw` for each shard.
Both require difficulty and subsidy rules even when staking is disabled:

| Field | Meaning and unit | Valid values |
| --- | --- | --- |
| `adjustmentCutoff` | Time scale in seconds for PoW difficulty adjustment. | Greater than zero. |
| `adjustmentFactor` | Divides parent difficulty to determine one adjustment step. | Greater than zero. |
| `minimumDifficulty` | Minimum PoW difficulty before staking discounts. | Positive `uint256`. |
| `coinbaseAmount` | Initial block subsidy before decay and reward tax. | `uint256`; zero disables the subsidy. |
| `epochInterval` | Blocks per reward and staking decay epoch. | Greater than zero. |
| `decayFactor` | Fraction retained per epoch; `0.88` retains 88%. | From zero to one. |

Amounts use the smallest native currency unit. The example has zero subsidies.
For shards, `config.quarkchain.rewardTaxRate` sets the fraction of the decayed
subsidy and each transaction fee shared with root. It must be greater than zero
and less than one. Root has no reward tax. Both `rewardTaxRate` and `decayFactor`
must have a finite decimal representation and a reduced denominator no greater
than 100.

`stakingEnabled` defaults to `false`. To enable the discount, provide positive
`windowSize`, `stakePerBlock`, and `diffDivider`. `windowSize` includes the candidate
block; `stakePerBlock` is the initial stake per block of quota; `diffDivider`
divides the PoW difficulty for a qualifying block.

For migrations, `originalBlockNumber` carries the old chain height into epoch
calculations. Enable `poswStakingDecayEnabled` to apply the same epoch and decay
factor to `stakePerBlock`. See [PoSWConfig](../../params/posw.go) for all fields.

Native QKC Genesis allocations must include the canonical shard staking contract
on every shard and the root staking contract on the first `chainIds` shard, even
when the local staking discount is disabled. The example allocations contain the
runtime code pinned in `params.PoSWStakingRuntimeCode`. The configuration loader
preserves allocations as supplied. The [Genesis constructor](../genesis) can add
required system-contract code before constructing blocks; it preserves supplied
balances, nonces and storage, and rejects conflicting code. The examples also
contain the QKC header extra data and encode
PoW difficulty in `mixHash`, with native `difficulty` and `nonce` set to zero.

## Configure processes in `cluster.toml`

Set `NetworkID` at the top level and `GenesisFile` on Master and every Slave.
The deployment must contain every Genesis membership chain exactly once.

- **Master** connects to the external P2P network. Configure process settings
  under `[Master.Node]` and external devp2p under `[Master.Node.P2P]`.
- **Each Slave** runs one shard. Set `ChainID` as a quoted decimal string, and
  `PrivateHost` / `PrivatePort` for internal TCP. Set `PublicHost` / `PublicPort`
  for HTTP JSON-RPC. Other process settings go under `[Slaves.Node]` and execution
  settings under `[Slaves.Eth]`.

See [node.Config](../../node/config.go) and
[ethconfig.Config](../../eth/ethconfig/config.go) for native settings. Omitted
execution settings inherit geth defaults. The example `DatabaseCache` values
illustrate independent per-shard overrides in MiB.

Use `--qkc.role master` to select Master, or
`--qkc.role slave --qkc.chain <id>` to select a Slave.

### Addresses, storage, and RPC

`PrivateHost` / `PrivatePort` define the internal TCP endpoint used both for
listening and for dialing by Master and other Slaves. The host must be concrete,
and the port between 1 and 65535. Use an unbracketed IPv6 host, such as `::1`.
Master derives its shard endpoint map from these fields.

`PublicHost` / `PublicPort` map to `Node.HTTPHost` / `Node.HTTPPort`, taking
precedence if both forms appear in TOML. An empty public host disables HTTP;
port zero requests a dynamically assigned port. WebSocket uses `Node.WSHost`
and `Node.WSPort`. Disabling HTTP or WebSocket leaves internal TCP configured.

Give each process its own data directory; configured directories must be distinct.
HTTP, WebSocket, and IPC settings are also independent for each process.

### Relative paths

- Each `GenesisFile` resolves relative to `cluster.toml`.
- Other paths retain geth's path handling. Relative `DataDir` resolves from the
  working directory. On Unix, a bare IPC filename resolves inside `DataDir`.

## Override process settings

Priority is explicit CLI flags, then the selected deployment entry, then defaults
for that role. Defaults reuse [node.DefaultConfig](../../node/defaults.go) and,
for shards, [ethconfig.Defaults](../../eth/ethconfig/config.go), with these changes:

- Separate data directories under geth's default data directory: `qkc/master`
  and `qkc/shard-<id>`.
- IPC filenames `qkc-master.ipc` and `qkc-shard-<id>.ipc`. On Unix, chain IDs above
  64 bits use a stable short suffix in default directories and IPC filenames.
- Shard RPC module lists include `eth`.
- External P2P is disabled for shards. No Ethereum bootstrap nodes or network
  presets are injected.

HTTP and WebSocket inherit disabled defaults. Use `--http` / `--ws` to enable
an endpoint and `--http=false` / `--ws=false` to disable one. An address flag
only changes an already enabled endpoint; it does not enable one by itself.

`checkconfig --help` lists supported overrides. Master rejects shard execution
flags such as `--syncmode`. Ethereum presets and unsupported flags are rejected.

A shard's effective `Eth.NetworkId` comes from deployment `NetworkID`. A nonzero
`Eth.NetworkId` override, or explicit `--networkid`, must match that value;
`--networkid=0` is rejected. Inline TOML `Eth.Genesis` and local fork overrides
are rejected: edit the referenced Genesis instead.

## Use the Go package

1. Call `LoadCluster` to decode deployment settings, apply defaults, and resolve
   Genesis file paths. It does not read any Genesis files.
2. Call `SelectNode` with the role and, for a shard, its chain ID.
3. Apply explicit process overrides to the returned `NodeConfig`.
4. To check only that process, call its `LoadGenesis()`, then
   `Validate(cluster.NetworkID)`.

For a complete deployment check, replace step 4 with `cluster.LoadGenesis()`,
then `cluster.Validate()`. This explicitly reads every referenced Genesis and
checks cross-shard consistency. `checkconfig` follows this complete sequence.

`SelectNode` returns the chosen entry within the deployment, so its edits are
included in validation. Validation also binds each shard's `Eth.NetworkId` to
the deployment network ID. The Master exposes its loaded consensus settings in
`NodeConfig.Root`; each Slave exposes its complete Genesis in `NodeConfig.Eth.Genesis`.
These checks do not replace native Genesis initialization or compatibility checks
against a stored chain configuration. Node startup remains a separate operation.

For read-only consensus configuration checks without process settings, use
`NodeConfig.ValidateGenesisConfig()` or `ClusterConfig.ValidateGenesisConfig()` on loaded
configurations. These do not validate genesis block fields or allocations, or
inspect network IDs, endpoints or data directories.

After `cluster.LoadGenesis()`, call `genesis.Build(cluster)` from
[qkc/genesis](../genesis) to construct blocks in memory. It consumes the prepared
root and shard definitions without reading files or modifying them, and returns
blocks in the shared `chainIds` order. The bundle retains the loaded definitions;
do not mutate them while the bundle is in use. Persistent initialization remains
separate.

## File formats

TOML follows geth's case-sensitive Go field names; unknown fields and duplicate
keys are errors. Deployment chain IDs are quoted decimal strings, such as
`ChainID = "100001"`.

Genesis uses native JSON field names and codecs. ChainConfig chain IDs, membership
entries, and fork heights are JSON integers. PoSW integers accept decimal numbers,
decimal strings, or hex strings; its ratios accept decimal numbers or numeric
strings. The examples use numbers for consensus settings. Genesis header values
and account allocations follow the native `core.Genesis` JSON format.
