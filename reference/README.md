# Protocol reference — where every design decision came from

This directory records the evidence behind the `meta` / `meta/dgw` packages. It is
here so the reasoning survives: the original artifact was a HAR-style capture of
meta.ai that only existed in a temp directory.

**Source artifact:** `https://files.catbox.moe/ao6pqa.zip` — a HAR-style dump of
meta.ai containing 158 minified Next.js chunks, CSS, i18n and HTML. It contains
**no original source code**, and the GraphQL responses in it were error documents.

## 1. GraphQL uses Relay persisted queries

`POST https://meta.ai/api/graphql` with:

```json
{"doc_id": "<32 hex>", "variables": { ... }}
```

Headers of note: `accept: multipart/mixed, application/json`.

`graphql-operations.txt` lists every operation recovered from the bundles
(`grep 'params:{id:"…"'` over the chunk files). 150 operations: 60 mutation,
86 query, 4 subscription.

Three doc_ids are used by this project and are pinned by a unit test in
`internal/meta/ops_test.go`:

| Operation | doc_id |
|---|---|
| `EctoHomePromptRouteQuery` | `816384a4f0a1c70e4631528a5ff36e8e` |
| `EctoMergedMessageListPreloadQuery` | `fc1d23c614253430e896695d71e80aaf` |
| `useMarkConversationLastSeenMessageMutation` | `0b2cb3a499606dfe326807a2b8b7ee20` |

**Multipart gotcha.** meta.ai declares the boundary as `-` (a single literal
character). The opening delimiter is therefore `--"-` and the **closing**
delimiter is `--"---`. A closing delimiter of `--"--` makes the second part fail
with `unexpected EOF` while the first part parses fine. Handled by
`resolveBoundary` in `internal/meta/graphql.go`.

## 2. Chat completion is NOT GraphQL

There is **no `SendMessageMutation`** anywhere in the capture. None of the 60
mutations sends a message; they are settings, feedback, and
conversation-management operations.

Sending a prompt and streaming the reply happens over a WebSocket to the DGW
gateway (`2umt4-28uxtjh.js`, `16n4cam67ld35.js`):

- Endpoint `wss://gateway.meta.ai/ws`, path suffixed with `/` + `serviceId`,
  where `serviceId` is `clippy`.
- Query parameters: `x-dgw-appid=1522763855472543`, `x-dgw-appversion=1.0.0`,
  `x-dgw-authtype=15:0` (the `ABRA` constant), `x-dgw-version=5`,
  `x-dgw-uuid=<fbId>`, `x-dgw-tier=prod`, `Authorization=<access token>`,
  `x-dgw-app-origin=meta.ai`, plus `x-dgw-establish-stream-frame-base64`.
- Grouping per conversation via header `x-ecto-conversation-id`.
- Timings: `connectTimeoutMs: 20000`, `keepAliveMs: 30000`.
- Payload keys assembled client-side: `conversationId`, `promptContent`,
  `userId`, `appId` (`1522763855472543`), `requestId`, `uniqueMessageId`,
  `turnId`, `agentType`, `agentTypeValue`, `attachments`, `mentionedData`,
  `promptChips`.

Full frame enum, read from the bundle:

| Frame | Value |
|---|---|
| `Empty` | 2 |
| `Drain` | 3 |
| `Deauth` | 4 |
| `DeprecatedEstabStream` | 5 |
| `DeprecatedData` | 6 |
| `SmallAck` | 7 |
| `DeprecatedEndOfData` | 8 |
| `Ping` | 9 |
| `Pong` | 10 |
| `StreamGroup_Ack` | 12 |
| `StreamGroup_Data` | 13 |
| `StreamGroup_EndOfData` | 14 |
| `StreamGroup_EstabStream` | 15 |

## 3. Why there is no hand-written codec

The bundle calls `__DgwCodecEncodeStreamGroup_Data`, `__DgwCodecDecode`,
`__DgwCodecEncodePing`, and friends. Those are wrappers over `r.asm.k`, `r.asm.l`,
… — a **WebAssembly module** called `dgwcppbridge.wasm`, driven through
`HEAPU8`, `_malloc`, and `WebAssembly.instantiate`.

That module is **not present in the capture**: the loader in `02h3llx9n0qf1.js`
resolves either an embedded data-URI (there is none — the only base64 data-URI in
the entire capture is a 1×1 GIF) or a network fetch that was not recorded.

Consequently the true wire encoding cannot be recovered from this artifact. A
hand-written codec would be an invention that passes its own round-trip tests and
fails against the real server. Instead:

- `internal/meta/dgw/frames.go` defines the enum and the `Codec` interface.
- `internal/meta/dgw/codec.go` holds **non-wire** test scaffolding only.
- `internal/meta/dgw/golden.go` compares against **recorded** bytes.
- `internal/meta/dgw/backend.go` returns `backend.ErrUnavailable` unless a token,
  a golden vector, and a codec are all supplied.
- `cmd/dgwprobe` records real sessions into the golden file.
- `internal/meta/dgw/codec/wazero` (task 11) bridges the real WASM, build-tagged
  `wasm_codec`, so the default build is unaffected while the asset is missing.

**To make the DGW backend real:** obtain `dgwcppbridge.wasm`, place it where
`make fetch-wasm` expects it, rebuild with `-tags wasm_codec`, then record a
session with `dgwprobe`. Until then the backend is off by default and the service
runs on `backend: mock`.

## 4. Authentication

- GraphQL: the `ecto_1_sess` cookie (plus `datr`, `wd`, `dpr`, `rd_challenge`).
- DGW: the same session, but the viewer access token is passed as the
  `Authorization` header value, and it is not derivable from the cookie alone.
