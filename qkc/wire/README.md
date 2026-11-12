# Adding wire messages

Define message schemas in `messages.go`. Field order is part of the SSZ wire
format. `messages_ssz.go` is generated from those schemas; do not edit it by
hand.

## Workflow

1. Add a unique opcode in `opcode.go` if the operation is new. Use `ClusterOp`
   for cluster RPCs and `CommandOp` for peer commands.
2. Add the message struct to `messages.go` with fields in protocol order. Add
   `ssz-max` tags to variable-length fields and document the inner codec of
   any `Data` field. Use the field rules below when choosing Go types.
3. Register the opcode and message type in `NewClusterMessage` or
   `NewCommandMessage` in `protocol.go`. Register both request and response
   opcodes for an RPC.
4. From the repository root, generate the SSZ codecs:

   ```sh
   go generate ./qkc/wire
   ```

   This generates `messages_ssz.go` using methodical-ssz and formats
   it with goimports. Both tools use the versions recorded in `go.mod`.
   Every struct declared in `messages.go` is included in generation.

5. Add registry, SSZ round-trip, malformed-input, and golden-encoding cases to
   `messages_test.go`, then run `go test ./qkc/wire`.
6. Commit `messages_ssz.go` together with the schema and test changes.

`make qkc`, `make all`, and `make test` also regenerate the wire codecs
automatically before building.

Direct `go build`, `go test`, and `go run build/ci.go install` commands do not
regenerate codecs. Run `go generate ./qkc/wire` first when using those commands
after changing `messages.go`.

## Field rules

- **Fixed values:** Use fixed-width unsigned integers and arrays, or `ssz-size:"N"` for
  a fixed-size slice. SSZ integers are little-endian; byte arrays are copied
  unchanged.
- **Byte lists:** Use `[]byte` or `Data` with `ssz-max:"N"`.
- **Lists of byte lists:** Use `[][]byte` with `ssz-max:"N,M"` for the schema
  bounds on element count and bytes per element. For `[]Data`, use `ssz-max:"N"` without
  `ssz-size`; N is the schema bound for both dimensions.
- **Container lists:** Use `[]*SlaveInfo` with `ssz-max:"N"`. Elements must be
  non-nil.
- **Optional values:** Encode presence explicitly, for example with a boolean.
- **Limits:** Validate received lengths in handlers; generated decoders do not
  enforce every byte-list bound. The existing `4294967295` tags are broad
  schema bounds; network payloads are limited to 16 MiB.

### Encoded objects: `Data`

Use `Data` or `[]Data` when an object cannot be encoded directly by the
generator. Encode the object with its native codec before putting the bytes in
the outer message: RLP for geth objects, SSZ for QKC root objects. For example,
use `rlp.EncodeToBytes(header)` for a geth header or `block.MarshalSSZ()` for
a QKC root block. The outer message's SSZ codec carries `Data` unchanged.

Document the inner codec beside each `Data` field. If construction is easy to
get wrong, add a helper alongside the message. The receiver must decode the
field with the same inner codec.

### Hashes and addresses

Use `common.Hash` for 32-byte hashes and `common.Address` for 20-byte
addresses. They are fixed byte arrays; do not reverse their byte order.

### Numeric uint256

Use `uint256.Int` for chain IDs, rewards and stakes. The generated SSZ codec
encodes these values as 32 little-endian bytes. Convert validated `*big.Int`
values with `uint256.MustFromBig` and convert back with `.ToBig()`. Shard chain
IDs must be positive and fit in 256 bits.

### Network IDs

`HelloCommand.NetworkID` is a `uint64` P2P network identifier, independent of
the uint256 chain ID. HELLO encodes it as eight little-endian bytes and has a
98-byte fixed section. This format is incompatible with peers using the old
four-byte NetworkID; all peers must use the same HELLO format.
