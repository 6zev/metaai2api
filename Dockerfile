# syntax=docker/dockerfile:1
#
# Multi-stage build for metaai2api.
#
# The default image carries no DGW codec. Build with
#   docker build --build-arg TAGS=wasm_codec .
# only when you have the module; that build FAILS when the asset is absent
# rather than shipping an image that silently lacks it.

ARG GO_VERSION=1.24.8

FROM golang:${GO_VERSION} AS build

WORKDIR /src

# Dependency layer: go.mod/go.sum first so source edits reuse the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TAGS=""
ARG VERSION=dev

# The wasm_codec build embeds internal/meta/dgw/codec/dgwcppbridge.wasm, which
# is a human prerequisite and not committed. Fail here, before compiling, so the
# error names the missing file instead of a go:embed message.
RUN set -eu; \
    case ",${TAGS}," in \
      *,wasm_codec,*) \
        test -s internal/meta/dgw/codec/dgwcppbridge.wasm || { \
          echo "error: TAGS=${TAGS} requested a wasm-enabled build," >&2; \
          echo "       but internal/meta/dgw/codec/dgwcppbridge.wasm is missing." >&2; \
          echo "       See internal/meta/dgw/codec/testdata/README.md." >&2; \
          exit 1; \
        } ;; \
    esac; \
    go build -trimpath \
      ${TAGS:+-tags ${TAGS}} \
      -ldflags "-s -w -X github.com/me/metaai2api/internal/version.Version=${VERSION}" \
      -o /out/metaai2api ./cmd/metaai2api

FROM debian:12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 65532 --home-dir /nonexistent --shell /usr/sbin/nologin metaai2api

COPY --from=build /out/metaai2api /usr/local/bin/metaai2api

# The updater refuses a non-executable target, so the file mode is explicit.
RUN chmod 0755 /usr/local/bin/metaai2api \
 && mkdir -p /etc/metaai2api \
 && chown metaai2api:metaai2api /etc/metaai2api

# The OTA updater replaces the binary at this path. Inside a container the running
# image is /proc/self/exe, which renaming over does not change what a restart
# runs, so main substitutes this path when it is a regular file. Leave the
# variable empty to disable OTA writes and keep the image read-only friendly.
ENV META_UPDATE_TARGET=/usr/local/bin/metaai2api

USER metaai2api
EXPOSE 8080

# No config file is baked in. Point -config at a mounted file when you have one;
# META_CONFIG does the same. Without either, defaults apply (backend: mock) and
# the usual environment overrides work.
ENV META_CONFIG=""
ENTRYPOINT ["/usr/local/bin/metaai2api"]
CMD []
