# QKC deployment configuration

## Start local devnet shards with geth

The [devnet](devnet) directory contains a self-contained geth configuration for
each shard, including its node settings, Genesis allocation and consensus rules.
Startup loads the configuration selected by `--config`. Both configurations use
geth's native `[Node]`, `[Eth]`, and `[Eth.Genesis]` sections. No `geth init` command is needed.

```sh
make geth

# Run either shard from the repository root.
build/bin/geth --config qkc/config/devnet/shard-100001.toml
build/bin/geth --config qkc/config/devnet/shard-100002.toml
```

| Configuration | Chain ID | HTTP endpoint | Data directory |
| --- | --- | --- | --- |
| [shard-100001.toml](devnet/shard-100001.toml) | 100001 | `127.0.0.1:18545` | `./qkc-data/shard-100001` |
| [shard-100002.toml](devnet/shard-100002.toml) | 100002 | `127.0.0.1:18546` | `./qkc-data/shard-100002` |

Each shard also has its own IPC filename. Runtime data under `qkc-data/` is ignored
by Git. With this binary on `PATH`, a configuration copied to `shard.toml` starts with
`geth --config shard.toml`. Relative data paths resolve from the working directory.

### Milestone 1: external mining

Build and start the local demo from the repository root:

```sh
make qkc
build/bin/qkc m1
```

By default, the driver starts Ethash shard 100001, constructs R0 from its genesis
header, and initializes the shard with that root. Use repeated `--config` flags
to explicitly select multiple shard configurations.
Ctrl-C closes the nodes. If any selected shard is past genesis, `qkc m1` exits
before initializing R0 on any shard. Use a fresh data directory for that shard.

QKC stores the genesis PoW difficulty in `[Eth.Genesis].Mixhash` as a 32-byte
hex value. Shard 100001 sets both the genesis and minimum difficulty to 1024,
so the adjustment step (`parentDifficulty / 512`) stays at least one.

In a second terminal, run the CPU test client to mine one real Ethash block:

```sh
# Optional endpoint and mining recipient overrides:
build/bin/qkc mine --rpc http://127.0.0.1:18545 --coinbase 0x222222222222222222222222222222222222222A
```

This is a single-block client for a low-difficulty devnet. The client fetches work, searches nonces with the existing light Ethash algorithm,
and submits a valid seal. It prints the work, seal and acceptance, then exits.

Shard 100001 uses `POW_ETHASH`; the optional shard 100002 uses `POW_SIMULATE`.
Local seal search is disabled. Root confirmation is manual:

```sh
build/bin/qkc m1 root --rpc http://127.0.0.1:18545
```

The command relays pending deposits between started shards, confirms their blocks
with one offline root, and returns the current root tip. With no pending blocks,
the tip stays unchanged. It accepts the HTTP endpoint of any shard started by the
same `m1` process. The mock cluster client continues to log calls.

The devnet HTTP configurations enable the `qkc` namespace. An external
miner requests work for its coinbase address:

```sh
curl -s http://127.0.0.1:18545 -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"qkc_getWork","params":["0x222222222222222222222222222222222222222A"],"id":1}'
```

The result contains `sealHash`, `number`, `difficulty`, and `time`. The three numeric
fields are 0x-prefixed hexadecimal strings. Use the epoch selected by `number`
and the effective PoSW `difficulty` to compute an Ethash seal over `sealHash`.
Submit the work hash, 8-byte hex nonce, and 32-byte hex mix hash, in that order:

```text
qkc_submitWork(["<sealHash>", "<0x-prefixed nonce>", "<0x-prefixed mix hash>"])
```

### Cross-shard transaction test

Run the self-contained two-shard test:

```sh
go test ./cmd/qkc -run '^TestM1XShardTransfer$' -count=1 -v
```

It checks a transfer from shard 100001 to 100002, root confirmation, balances and
fees, and that repeated deposit or root delivery does not credit the recipient twice.

For manual testing, start both shards:

```sh
build/bin/qkc m1 --config qkc/config/devnet/shard-100001.toml \
  --config qkc/config/devnet/shard-100002.toml
```

In another terminal, send 1 QKC from shard A (100001) to an address on shard B
(100002), using the funded developer test account:

```sh
XSHARD_A_RPC=http://127.0.0.1:18545
XSHARD_B_RPC=http://127.0.0.1:18546
XSHARD_TO=0x00000000000000000000000000000000000000be
XSHARD_TEST_KEY=0xb71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291

cast balance "$XSHARD_TO" --rpc-url "$XSHARD_B_RPC"
XSHARD_FROM=$(cast wallet address --private-key "$XSHARD_TEST_KEY")
XSHARD_NONCE=$(cast nonce "$XSHARD_FROM" --block pending --rpc-url "$XSHARD_A_RPC")
```

Choose one of the following transaction examples; each sends 1 QKC with the same
effective gas price and gas costs.

Legacy:

```sh
XSHARD_TX=$(cast send 0x000000000000000000000000000000514b430100 \
  'send(uint256,address,uint256,uint64,bool,bytes)' \
  100002 "$XSHARD_TO" 1000000000000000000 9000 true 0x \
  --rpc-url "$XSHARD_A_RPC" --chain 100001 \
  --private-key "$XSHARD_TEST_KEY" --nonce "$XSHARD_NONCE" \
  --legacy --gas-price 2000000000 --gas-limit 120000 \
  --value 1000018000000000000 --async)
```

The `send` value is 1 QKC (`10^18` base units). The call's `--value` also prepays
destination gas: `10^18 + 9000 * 2000000000 = 1000018000000000000`. Source transaction
gas is charged separately. `--legacy` fixes the effective gas price at 2 gwei;
`--async` returns the transaction hash before it is mined.

EIP-1559 (use instead of the legacy example, with the same variables):

```sh
XSHARD_TX=$(cast send 0x000000000000000000000000000000514b430100 \
  'send(uint256,address,uint256,uint64,bool,bytes)' \
  100002 "$XSHARD_TO" 1000000000000000000 9000 true 0x \
  --rpc-url "$XSHARD_A_RPC" --chain 100001 \
  --private-key "$XSHARD_TEST_KEY" --nonce "$XSHARD_NONCE" \
  --gas-price 2000000000 --priority-gas-price 2000000000 --gas-limit 120000 \
  --value 1000018000000000000 --async)
```

Here `--gas-price` sets `maxFeePerGas` and `--priority-gas-price` sets
`maxPriorityFeePerGas`. Both are 2 gwei, so the effective price is
`min(2 gwei, baseFee + 2 gwei) = 2 gwei` whenever the transaction can be included,
matching the legacy example. The transfer value, prepaid destination gas, and
source gas charge are therefore unchanged.

After sending either transaction, wait for the sender's pending nonce to advance
on shard A. Then mine the source block and check the receipt.

```sh
(
  for attempt in $(seq 15); do
    pending_nonce=$(cast nonce "$XSHARD_FROM" --block pending --rpc-url "$XSHARD_A_RPC") || exit 1
    if [ "$pending_nonce" -gt "$XSHARD_NONCE" ]; then
      build/bin/qkc mine --rpc "$XSHARD_A_RPC" &&
        cast receipt "$XSHARD_TX" --rpc-url "$XSHARD_A_RPC"
      exit $?
    fi
    sleep 1
  done
  echo "Timed out waiting for the sender's pending nonce to advance" >&2
  exit 1
)
```

Explicitly confirm A1 in R1 (on a fresh run), then mine on B and check the recipient:

```sh
build/bin/qkc m1 root --rpc "$XSHARD_A_RPC"
build/bin/qkc mine --rpc "$XSHARD_B_RPC"
cast balance "$XSHARD_TO" --rpc-url "$XSHARD_B_RPC"
```

The balance increases by `1000000000000000000` base units.

### Test accounts

Both shards preallocate 1,000,000 QKC (`1000000000000000000000000` base units) to
each account below. These are public test keys; use them only for local development.

| Account | Address | Test private key |
| --- | --- | --- |
| Geth developer account | `0x71562b71999873DB5b286dF957af199Ec94617F7` | `b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291` |
| Second test account | `0x2B5AD5c4795c026514f8317c7a215E218DcCD6cF` | `0000000000000000000000000000000000000000000000000000000000000002` |

Genesis allocations fund addresses; they do not import or unlock keys. Sign test
transactions with these keys and the target shard's chain ID. For example, query
the first account's balance on chain 100001:

```sh
curl -s http://127.0.0.1:18545 -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x71562b71999873DB5b286dF957af199Ec94617F7","latest"],"id":1}'
```

Adding or changing allocations changes the Genesis hash. If you initialized an
earlier example, start with a new data directory (using `--datadir`) or back up and
recreate the old one; restarting an existing chain does not apply new allocations.

### Configuration and startup

The first start writes Genesis, its state, and the chain configuration to the
database. Subsequent starts parse the TOML again and check its Genesis against the
database. After initialization, omitting `[Eth.Genesis]` and its subtables loads
the existing chain from the same data directory. An incompatible Genesis or
changed QuarkChain configuration fails startup.

This starts the local blockchain with the existing PoSW engine and geth's node
lifecycle and RPC services.

Set `ConsensusType` under `[Eth.Genesis.Config.QuarkChain]` to `"POW_ETHASH"`
(shard 100001) or `"POW_SIMULATE"` (shard 100002). Both use PoSW rules; simulation
skips Ethash seal verification. Omitting the field retains the legacy Ethash
behavior. Unknown values, including `"NONE"`, are rejected.

The consensus type is stored with the chain configuration and must match on
restart, even though changing it does not change the Genesis hash. An omitted
value and an explicit `"POW_ETHASH"` are distinct stored configurations.
For an existing database initialized without this field, keep omitting it or use
a new data directory with the updated examples. A database-only restart uses the
stored consensus type.

TODO: initialize `ShardChain` from a Master root checkpoint and connect
cluster transport. 

TOML uses case-sensitive Go field names, quoted big integers and rationals. `geth dumpconfig`
omits Genesis, so keep the original configuration for initializing other nodes.

## Build the devnet root genesis

[devnet/root.toml](devnet/root.toml) references the two native shard TOMLs used
by geth. `Root.ShardConfigs` paths resolve relative to the root TOML. Each shard
supplies its own genesis allocation. All shards must agree on the network ID,
ordered `ChainIDs`, `RewardTaxRate` and `MaxBlocksPerRootBlock`. R0 includes one
full shard genesis header per chain in `ChainIDs` order. `Root.Genesis.Coinbase`
defaults to the zero address in the R0 header. `Node.CoinbaseAddress` configures
the local mining recipient.

Load the file with `config.LoadRoot(path)` and call
`root.BuildRootGenesis(cfg.Genesis, cfg.ShardGeneses())` to derive R0 offline.

Set both `Node.PrivateHost` and `Node.PrivatePort` to configure the root private
RPC listener. The root can bind a wildcard address. Shard endpoints require
concrete hosts for dialing. Each configured private endpoint must be distinct.

## Initialize a local root database

Open the dedicated root database and call
`root.InitializeRootDatabase(db, cfg.Genesis, cfg.ShardGeneses())`. On first use,
it writes R0, its canonical mapping, head, shard confirmations and root rules in
one batch. On restart, it verifies R0 and the initial confirmations and returns
the effective rules and R0 hash. If the database has a head, it must also have
a canonical R0 mapping.

An initialized database also accepts a TOML with the entire `[Root.Genesis]`
section and its subtables omitted. The referenced shard TOMLs must still agree
with the persisted rules and G0 headers. Use the returned rules to create the
root consensus engine and call `root.New`, which validates and restores the
persisted head. Initialization is local and can run before a cluster connection
exists.
