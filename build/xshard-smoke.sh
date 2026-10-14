#!/usr/bin/env bash
# Run from the repository root after make geth. Requires cast, jq and curl.
set -euo pipefail

QKC_GETH="${QKC_GETH:-./build/bin/geth}"
QKC_SMOKE_DIR="${QKC_SMOKE_DIR:-$(mktemp -d)}"
S_PORT="${S_PORT:-18545}"
D_PORT="${D_PORT:-18546}"
S_RPC="http://127.0.0.1:$S_PORT"
D_RPC="http://127.0.0.1:$D_PORT"
OUTBOX=0x000000000000000000000000000000514b430100
S_PID=
D_PID=

log() {
    printf '[smoke] %s\n' "$*"
}

print_message() {
    local event="$1" event_data payload
    # topics[1..3] contain dstShard/from/to; data contains value/gasPrice/gasLimit/bytes.
    event_data="$(jq -r .data <<< "$event")"
    payload="$(cast abi-decode 'message()(uint256,uint256,uint64,bytes)' "$event_data" --json | jq -r '.[3]')"
    # Keep uint256 values as decimal strings to avoid jq numeric precision loss.
    jq --arg dstShard "$(cast to-dec "$(jq -r '.topics[1]' <<< "$event")")" \
        --arg value "$(cast to-dec "0x${event_data:2:64}")" \
        --arg gasPrice "$(cast to-dec "0x${event_data:66:64}")" \
        --arg gasLimit "$(cast to-dec "0x${event_data:130:64}")" \
        --arg data "$payload" '
        {message: {sourceShard: 0, dstShard: $dstShard,
            from: ("0x" + .topics[2][-40:]), to: ("0x" + .topics[3][-40:]),
            value: $value, gasPrice: $gasPrice, gasLimit: $gasLimit, data: $data},
         sourceLog: .}
    ' <<< "$event"
}

cleanup() {
    local status=$?
    log "Stopping test nodes..."
    for pid in "$D_PID" "$S_PID"; do
        if [[ -n "$pid" ]]; then
            kill -TERM "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    if [[ "$status" != 0 ]]; then
        tail -n 20 "$QKC_SMOKE_DIR"/*.log
    fi
    echo "Artifacts: $QKC_SMOKE_DIR"
}
# Stop only the processes started by this script, and keep their data/logs for inspection.
trap cleanup EXIT

wait_rpc() {
    local rpc_started=$SECONDS
    for ((i=0; i<60; i++)); do
        if cast rpc eth_chainId --rpc-url "$1" >/dev/null 2>&1; then
            log "RPC ready: $1"
            return
        fi
        if ((i % 10 == 0)); then
            log "Waiting for RPC: $1 ($((SECONDS-rpc_started))s elapsed)"
        fi
        sleep 1
    done
    echo "RPC unavailable: $1" >&2
    return 1
}

# 1. Use isolated data directories and check that neither RPC port is already in use.
log "Artifacts: $QKC_SMOKE_DIR"
for endpoint in "$S_RPC" "$D_RPC"; do
    if curl -s --connect-timeout 1 --max-time 1 "$endpoint" >/dev/null; then
        echo "Port already in use: $endpoint; set S_PORT and D_PORT." >&2
        exit 1
    fi
done

# 2. Create S (shard 0) and D (shard 1), with a funded public test EOA.
log "Preparing genesis: S=shard 0, D=shard 1"
"$QKC_GETH" --dev dumpgenesis 2>"$QKC_SMOKE_DIR/genesis.log" | jq '
    .config.quarkchain = {shardId: 0, shardIds: [0, 1]}
    | .config.amsterdamTime = 0 | del(.config.bogotaTime)
    | .alloc["7e5f4552091a69125d5dfcb7b8c2659029395bdf"] = {balance: "1000000000000000000000"}
' > "$QKC_SMOKE_DIR/S.json"
jq '.config.chainId = 1338 | .config.quarkchain.shardId = 1' \
    "$QKC_SMOKE_DIR/S.json" > "$QKC_SMOKE_DIR/D.json"

for shard in S D; do
    log "Initializing $shard database..."
    "$QKC_GETH" --datadir "$QKC_SMOKE_DIR/$shard" --cache 128 init "$QKC_SMOKE_DIR/$shard.json" \
        >"$QKC_SMOKE_DIR/$shard-init.log" 2>&1
done
# D reads source shard 0's Outbox through S's RPC and saves messages locally.
cat > "$QKC_SMOKE_DIR/D.toml" <<EOF
[Eth.XShardSource]
ShardID = 0
RPC = "$S_RPC"
EOF

# 3. Start both nodes; dev mode produces blocks automatically every second.
log "Starting S: RPC=$S_RPC, log=$QKC_SMOKE_DIR/S.log"
"$QKC_GETH" --datadir "$QKC_SMOKE_DIR/S" --dev --dev.period 1 --cache 128 \
    --http --http.addr 127.0.0.1 --http.port "$S_PORT" --http.api eth \
    --authrpc.port "$((S_PORT+6))" --ipcdisable >"$QKC_SMOKE_DIR/S.log" 2>&1 &
S_PID=$!
start_destination() {
    log "Starting D: RPC=$D_RPC, log=$QKC_SMOKE_DIR/D.log"
    "$QKC_GETH" --datadir "$QKC_SMOKE_DIR/D" --config "$QKC_SMOKE_DIR/D.toml" \
        --dev --dev.period 1 --cache 128 \
        --http --http.addr 127.0.0.1 --http.port "$D_PORT" --http.api eth,qkc \
        --authrpc.port "$((D_PORT+6))" --ipcdisable >>"$QKC_SMOKE_DIR/D.log" 2>&1 &
    D_PID=$!
    wait_rpc "$D_RPC"
}
wait_rpc "$S_RPC"
start_destination

# 4. Send ONE transaction to S's Outbox (--rpc-url S_RPC).
# The first send argument is dstShard=1 (D); the following address is the recipient on D.
# --value prepays 12345 + 100000 * 2 gwei; the public private key is for local tests only.
log "Sending on S: dstShard=1, value=12345, gasLimit=100000, data=0xcafe"
cast send "$OUTBOX" 'send(uint32,address,uint256,uint64,bytes)' \
    1 0x2000000000000000000000000000000000000002 12345 100000 0xcafe \
    --value 200000000012345 --gas-limit 300000 --gas-price 2000000000 --legacy \
    --private-key 0x0000000000000000000000000000000000000000000000000000000000000001 \
    --rpc-url "$S_RPC" --json > "$QKC_SMOKE_DIR/receipt.json"
jq -e --arg outbox "$OUTBOX" '.status == "0x1" and ([.logs[] | select(.address == $outbox)] | length == 1)' \
    "$QKC_SMOKE_DIR/receipt.json" >/dev/null
jq -r '"[smoke] S receipt: tx=\(.transactionHash) block=\(.blockNumber) status=\(.status) gasUsed=\(.gasUsed)"' \
    "$QKC_SMOKE_DIR/receipt.json"
log "S Outbox message:"
print_message "$(jq -c --arg outbox "$OUTBOX" '.logs[] | select(.address == $outbox)' "$QKC_SMOKE_DIR/receipt.json")"

# 5. Wait for D to save the event. Dev finality advances every 32 source blocks.
BLOCK_HASH="$(jq -r .blockHash "$QKC_SMOKE_DIR/receipt.json")"
log "Waiting for D to persist source block $BLOCK_HASH (finalized blocks only)..."
receive_started=$SECONDS
for ((i=0; i<90; i++)); do
    cast rpc qkc_getXShardMessages 0 "$BLOCK_HASH" --rpc-url "$D_RPC" > "$QKC_SMOKE_DIR/received.json"
    if jq -e 'type == "array" and length == 1' "$QKC_SMOKE_DIR/received.json" >/dev/null; then
        break
    fi
    if ((i % 10 == 0)); then
        log "Waiting for D: $((SECONDS-receive_started))s elapsed"
    fi
    sleep 1
done
# 6. Compare the saved event with S's Outbox event, excluding unrelated receipt logs.
diff -u <(jq -S '.[0]' "$QKC_SMOKE_DIR/received.json") \
    <(jq -S --arg outbox "$OUTBOX" '.logs[] | select(.address == $outbox)' "$QKC_SMOKE_DIR/receipt.json")
log "D stored one event matching the S Outbox event"
print_message "$(jq -c '.[0]' "$QKC_SMOKE_DIR/received.json")"

# 7. Stop S and restart D with the same database: the event must remain readable offline.
log "Stopping S and D, then restarting D with S offline..."
kill -INT "$D_PID" "$S_PID"
wait "$D_PID"
wait "$S_PID"
D_PID=
S_PID=
start_destination
cast rpc qkc_getXShardMessages 0 "$BLOCK_HASH" --rpc-url "$D_RPC" > "$QKC_SMOKE_DIR/restarted.json"
diff -u "$QKC_SMOKE_DIR/received.json" "$QKC_SMOKE_DIR/restarted.json"
log "Persistence verified: the same event is readable after restarting D"
print_message "$(jq -c '.[0]' "$QKC_SMOKE_DIR/restarted.json")"
echo "PASS: S -> D received one message and preserved it across restart with S offline."
