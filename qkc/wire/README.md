# SSZ message workflow

`messages.go` contains the wire message schemas. Field order is part of each
SSZ container. `messages_ssz.go` is generated code and must not be edited by
hand.

## Add a message

1. Add a Go struct to `messages.go` with fields in protocol order. Use fixed
   width integers and arrays for fixed-size values, `[]byte` for byte strings,
   `Data` for opaque serialized values, and `[]T` for variable lists. For nested
   variable lists, use one limit per dimension and `ssz-size:"?,?"`.
2. For variable-length fields, add `ssz-max` with a numeric limit. Use a
   protocol-defined limit when one is known, or the current broad limit
   `4294967295` otherwise. Use fixed Go types or `ssz-size:"N"` for
   fixed-size fields.
3. Add the opcode-to-type case to `NewClusterMessage` or
   `NewCommandMessage` in `protocol.go`.
4. From the repository root, run:

   ```sh
   go generate ./qkc/wire
   ```

5. Add codec round-trip, malformed-input, and golden-encoding cases to
   `messages_test.go`.

The `go:generate` directive runs `sszgen` against the message schemas. The
generator scans their structs; named types from other packages must be made
available through its `--include` option when the schema uses them.

Use `Data` or `[]Data` for objects the generator cannot encode directly: callers
encode them with their native codec (RLP for geth, SSZ for QKC root objects),
and SSZ carries the resulting bytes unchanged.

## SSZ field rules

- `uintN` and `[N]byte` encode as fixed-size values.
- `[]byte` and `Data` are `ByteList` values; their actual length is determined
  by SSZ offsets and the enclosing message boundary.
- `[]T` is a `List[T]`; `[]Data` is a list of byte lists. Each dynamic list
  uses offsets to locate its elements.
- SSZ integers and offsets are little-endian. The transport layer carries the
  encoded SSZ bytes as the message payload; byte arrays are copied unchanged.
- SSZ has no implicit nullable Go pointer. Represent presence explicitly in
  the schema and test both present and absent cases.

For nested variable lists, provide one `ssz-max` value per dimension and use
`ssz-size:"?,?"`. The current `4294967295` tags are broad placeholders and can
be revisited later; see the TODO in `messages.go`.
