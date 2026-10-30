# metaai2api

An OpenAI-compatible HTTP API in front of a chat backend. Today that backend is
`mock`; the `experimental-dgw` backend that talks to meta.ai's gateway is **built
but unverified** — see [Experimental backend](#experimental-backend--unverified-against-live-metaai).

**Honest status**

| Area | State |
|---|---|
| HTTP surface (`/v1`, admin) | Implemented and tested |
| `mock` backend | Default, deterministic, no credentials needed |
| meta.ai GraphQL client | Implemented; multipart framing covered by tests |
| `experimental-dgw` backend | **Does not work against meta.ai yet.** `dgwcppbridge.wasm` is absent, so no frame is ever encoded or decoded against the real gateway |
| Proxy, updater, OTA | Implemented and tested |

Nothing here has been run against a live meta.ai session. The evidence everything
is based on — a HAR-style capture of the site — is written up in
[`reference/README.md`](reference/README.md). Read that before trusting any claim
below about the protocol.

## Quick start

```sh
make build
META_COOKIE='datr=placeholder' ./metaai2api -config config.example.yaml
```

```sh
curl -s localhost:8080/admin/health
```

The default backend is `mock`, so no meta.ai account is involved.

```sh
curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"meta-think-fast","stream":false,
       "messages":[{"role":"user","content":"hello"}]}'
```

Streaming works the same way with `"stream":true`: the reply is Server-Sent
Events, one `chat.completion.chunk` per delta, ending with `data: [DONE]`.

## Configuration

Configuration comes from a YAML file (`-config`, default is no file; `$META_CONFIG`
sets it instead) plus environment overrides. Environment wins.

| YAML key | Env override | Meaning |
|---|---|---|
| `listen` | — | listen address, default `:8080` |
| `backend` | `BACKEND` | `mock` (default) or `experimental-dgw` |
| `cookie` | `META_COOKIE` | meta.ai `Cookie` header |
| `access_token` | `META_ACCESS_TOKEN` | DGW viewer access token |
| `api_token` | `API_TOKEN` | Bearer token required on `/v1/*`; empty is open |
| `admin_token` | `ADMIN_TOKEN` | Bearer token for `/admin/*`; empty removes those routes (404) |
| `agent_type` | — | `think_fast` or `think_hard` |
| `proxy_url` | `ALL_PROXY`, then `HTTP_PROXY` | outbound proxy |
| `updater.*` | — | OTA updater, see below |

Secrets belong in the environment. Startup logs the configuration with every
secret replaced by `***`; the cookie keeps its key names only so you can see
which cookies are set.

`config.example.yaml` documents every key.

## HTTP API

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI chat completions, stream and non-stream |
| `GET /v1/models` | `meta-think-fast`, `meta-think-hard` |
| `POST /v1/images/generations` | `501` — the capture exposes no image endpoint |
| `GET /admin/health` | Public. `ok`, `version`, `backend`, last update check |
| `GET`/`PUT /admin/config` | Admin only. `PUT` is all-or-nothing |
| `POST /admin/update` | Admin only. Runs one OTA check |
| `GET /admin/version` | Admin only |

Admin routes are registered only when `admin_token` is set. Without it every
`/admin/*` path except `/admin/health` is `404` — closed by default, not merely
unauthorized.

Error mapping from the backend: session expired → `401`, unavailable → `503`,
not implemented → `501`, protocol error → `502`, anything else → `500`.

## Credentials

Both are browser values and both expire; nothing here derives one from the other.

- **Cookie** (`META_COOKIE`): the `Cookie` header meta.ai sends — `ecto_1_sess`
  (the session) plus `datr`, `wd`, `dpr`, `rd_challenge`. Copy it from devtools →
  Network → any `api/graphql` request.
- **Access token** (`META_ACCESS_TOKEN`): the DGW viewer access token, sent as the
  WebSocket `Authorization` query parameter. It is **not** derivable from the
  cookie. It appears in the capture's WebSocket handshake; see
  `reference/README.md` §4.

Only the `experimental-dgw` backend uses the access token. `mock` uses neither.

## Proxy

Upstream traffic (GraphQL, WebSocket dial, OTA downloads) routes through
`proxy_url`, or `ALL_PROXY` / `HTTP_PROXY` when that is empty. Schemes: `http`,
`https`, `socks5`, `socks5h` (`socks5h` resolves names at the proxy). The
WebSocket dial needs `socks5`/`socks5h`; an `http`/`https` proxy is a loud error
for that path rather than a silent direct connection.

## OTA updater

Disabled by default. When enabled it queries GitHub Releases and replaces the
binary only after the downloaded SHA256 matches `checksums.txt`; a missing
checksums file, a missing asset, a mismatch, or a non-executable target leaves
the running binary byte-for-byte intact.

```yaml
updater:
  enabled: true
  owner: you
  repo: metaai2api
  interval: 24h          # non-positive disables the poller
  asset_pattern: "metaai2api-{os}-{arch}"
```

- `POST /admin/update` runs one check on demand; `GET /admin/health` reports the
  last one.
- The poller starts only when the updater is enabled **and** configured.
- The running binary must be writable by the process. In a container the running
  image is `/proc/self/exe`, which renaming over does not change what a restart
  runs, so the Docker image sets `META_UPDATE_TARGET` to the on-disk path and the
  updater writes there instead.

## Build

```sh
make build        # default build, no WASM codec
make test
make check        # gofmt + vet + test
```

`VERSION` is stamped into `internal/version.Version`:

```sh
make build VERSION=v1.2.3
```

which is `-ldflags "-X github.com/me/metaai2api/internal/version.Version=v1.2.3"`.
That value is what `/admin/health` reports and what the updater compares.

## Experimental backend — unverified against live meta.ai

**This backend does not work against meta.ai as shipped.** It is here because the
protocol is documented and partly implemented, not because it has been shown to
work.

Why:

- Sending a prompt is not GraphQL — it is a WebSocket to `wss://gateway.meta.ai/ws`
  (service `clippy`). See `reference/README.md` §2.
- Frames are encoded and decoded by `dgwcppbridge.wasm`, a WebAssembly module the
  site loads. **That module is not in the published capture**, so its bytes are
  unknown. A hand-written codec would pass its own round-trip tests and fail
  against the real server, so none was written.
- `internal/meta/dgw` therefore refuses to start without an access token, a
  golden vector **and** a codec, and `Stream`/`Complete` return
  `ErrNotImplemented` with a real codec absent.

Selecting it without its prerequisites is a startup failure that names every
missing piece. **It never silently falls back to `mock`.**

```sh
BACKEND=experimental-dgw ./metaai2api
# backend experimental-dgw cannot start: missing access token (...); golden
# vector (...); WASM codec (...) ... there is no fallback to mock
```

### The `dgwprobe` → golden → `wasmcheck` workflow

The order matters: each step produces the input for the next.

1. **Obtain `dgwcppbridge.wasm`.** Load meta.ai with the network panel open,
   filter for `wasm`, and save the `dgwcppbridge.wasm` response. Then either
   ```sh
   make fetch-wasm SRC=/absolute/path/to/dgwcppbridge.wasm
   ```
   or point `DGW_CODEC_WASM` at an absolute path. The file is git-ignored and is
   not committed at any point (unknown licence and provenance).
2. **Check the bridge:**
   ```sh
   make wasmcheck
   ```
   Instantiates the module, lists its exports, and round-trips a
   `StreamGroup_Data` frame. Exits `0` on PASS, `1` on a module that loads but
   disagrees with itself, `2` when no module can be bound.
3. **Record a session:**
   ```sh
   META_COOKIE='...' META_ACCESS_TOKEN='...' go run ./cmd/dgwprobe \
     -conversation '...' -seconds 30 -out golden.json
   ```
   This dials the gateway and records frames; `-out` writes the golden vector
   `updater.dgw_golden_path` expects (without it, frames are only printed).
4. **Start the backend** with `backend: experimental-dgw` and
   `updater.dgw_golden_path: golden.json`.

Steps 3 and 4 are the untested part: no real module and no recorded session
exist in this repository, and `Stream` is unimplemented even with a codec bound.
Details and the exact ABI the bridge assumes are in
[`internal/meta/dgw/codec/testdata/README.md`](internal/meta/dgw/codec/testdata/README.md).

## Docker

```sh
docker build -t metaai2api .
docker run --rm -p 8080:8080 metaai2api
```

The default image has no DGW codec. A wasm-enabled image must be built from a
checkout that contains the module:

```sh
make fetch-wasm SRC=/absolute/path/to/dgwcppbridge.wasm
docker build --build-arg TAGS=wasm_codec -t metaai2api:wasm .
```

The build **fails** when `TAGS=wasm_codec` is requested and the asset is absent,
rather than shipping an image that silently lacks it. `--build-arg VERSION=v1.2.3`
stamps the version.

## Layout

```
cmd/metaai2api     wiring: flags, backend selection, graceful shutdown
cmd/dgwprobe       records DGW frames into a golden vector
cmd/wasmcheck      smoke test for the DGW WASM codec
internal/backend   ChatBackend interface and typed errors
internal/config    YAML + env configuration, redaction
internal/meta      meta.ai GraphQL client (Relay persisted queries)
internal/meta/dgw  experimental DGW protocol harness (frames, golden vectors)
internal/meta/dgw/codec  wazero bridge to dgwcppbridge.wasm (build-tagged)
internal/meta/mock deterministic default backend
internal/openai    OpenAI-compatible handlers
internal/proxy     outbound proxy transports and dialers
internal/server    middleware, admin API, mounting
internal/updater   OTA check and fail-closed self-replace
internal/version   the ldflags version variable
reference          the capture analysis every protocol decision came from
```
