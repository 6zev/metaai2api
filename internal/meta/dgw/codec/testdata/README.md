# Supplying dgwcppbridge.wasm

Task 11's bridge (`internal/meta/dgw/codec/wazero.go`) drives the real DGW
protobuf codec, compiled to WebAssembly. **The module is not in this
repository**: the meta.ai capture in `reference/` resolves it by URL in
`02h3llx9n0qf1.js` (`M="dgwcppbridge.wasm"`) but never records the bytes, and its
licence and provenance are unknown.

Until the module is present, everything below is inert:

- the default build has no codec; `codec.New()` returns an error wrapping
  `backend.ErrUnavailable`;
- `cmd/wasmcheck` builds and prints a diagnostic, exiting 2;
- `go test ./...` is green.

None of this is verified for interoperability with the live gateway.

## Getting the module

1. Load meta.ai with the browser network panel open, filter for `wasm`, and save
   the `dgwcppbridge.wasm` request (it is served from `/_next/static/...` under a
   build hash). Alternatively grab it from a local Next.js build cache.
2. Put an absolute path to the file in `DGW_CODEC_WASM`, or drop it into this
   directory as `dgwcppbridge.wasm`.

   ```sh
   export DGW_CODEC_WASM=/absolute/path/to/dgwcppbridge.wasm
   # or
   make fetch-wasm SRC=~/Downloads/dgwcppbridge.wasm
   ```

`make fetch-wasm` copies from `SRC` (a URL is fetched with `curl`) into this
directory. The file is git-ignored; `embed_codec.go` embeds it and is the only
file that does, so a checkout without it still builds.

## Building and checking

```sh
go build -tags wasm_codec ./cmd/wasmcheck
./wasmcheck                 # lists exports, round-trips a StreamGroup_Data frame
go test -tags wasm_codec ./internal/meta/dgw/codec/ -v
```

`wasmcheck` exits 0 on PASS, 1 when the module loads but disagrees with itself
(including a missing `__DgwCodec*` export, which the bridge reports as a
`codec.MissingSymbolError`), and 2 when no module can be bound. It never panics.

## What the bridge uses

From `02h3llx9n0qf1.js` (loader) and `2umt4-28uxtjh.js` (call sites):

| Export | Signature used | Called by |
|---|---|---|
| `___wasm_call_ctors` | `()` | constructor, run once after instantiate |
| `__malloc` / `__free` | `(size) -> ptr` / `(ptr)` | every operation |
| `__DgwCodecEncodeEmpty` | `(out)` | `FrameEmpty` |
| `__DgwCodecEncodeDeauth` | `(out)` | `FrameDeauth` |
| `__DgwCodecEncodeDrain` | `(reason, out)` | `FrameDrain` |
| `__DgwCodecEncodePing` / `__DgwCodecEncodePong` | `(out)` | heartbeat |
| `__DgwCodecEncodeStreamGroup_Ack` | `(streamId, ackId, out, outLen)` | `FrameStreamGroupAck` |
| `__DgwCodecEncodeStreamGroup_Data` | `(streamId, ptr, len, req, ack, out, outLen)` (the site passes `req=false, ack=0`; acking is the site's manager, not the codec) | `FrameStreamGroupData` |
| `__DgwCodecEncodeStreamGroup_EndOfData` | `(streamId, out)` | `FrameStreamGroupEndOfData` |
| `__DgwCodecEncodeStreamGroup_EstabStream` | `(streamId, ptr, len, out, outLen)` | `FrameStreamGroupEstabStream` |
| `__DgwCodecDecode` | `(in, inLen, framePtrs, frameCount, consumed)` | every read |
| `__getDGWFramePtr`, `__getFrameType`, `__getStreamIdFromStreamGroupFrame`, `__getDataFromGroupedStreamDataFrame`, `__getDataSizeFromGroupedStreamDataFrame`, `__getRequiresAckFromGroupedStreamDataFrame`, `__getAckIdFromGroupedStreamDataFrame`, `__getAckIdFromGroupedStreamAckFrame`, `__getEncodedParamsFromEstablishStreamFrame`, `__getEncodedParamsSizeFromEstablishStreamFrame`, `__getDrainReasonFromDrainFrame` | see `readFrame` | decode result walk |

`out` is a two-uint32 field pair: pointer at +0, length at +4. The site always
allocates it as one 8-byte buffer and splits it, which the bridge does too. The
empty/deauth/ping/pong exports take only the first half and read the second at
`out+4`. The module's memory is exported as `memory`; the loader reads it as
`HEAPU8`.

The loader's JS argument structs (`{tag, uint32, bytes}`) are transpiled into
these flat signatures, so the bridge passes flat arguments, not Ptr/len pairs in
a struct. The functions take i32 parameters: the JS wrappers mask them with
`|0`, which is why the bridge passes them as I32 arguments.

## Known gaps

- The bridge has never run against the real module. It is exercised against a
  fake `api.Module` with the same export names (`wazero_test.go`) to prove
  binding, memory handling and error reporting, which is not the same as a wire
  round-trip.
- `__DgwCodecDecode`'s output struct layout is read as
  `+0 frames, +4 count, +8 consumed`. The site's code is consistent with that
  reading, but nothing confirms it against the module.
- Return `1` is accepted alongside `0` on decode: the site's decode wrapper
  treats the low bit of the trailing-segment split as a completeness marker. The
  encoder paths accept `0` and `1` for the same reason.
