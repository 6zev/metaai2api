# metaai2api build targets.
#
# The DGW codec module (dgwcppbridge.wasm) is not in this repository; see
# internal/meta/dgw/codec/testdata/README.md for why and how to supply it.

GO ?= go

# VERSION is stamped into internal/version.Version and is what the OTA updater
# compares against. Override it: make build VERSION=v1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

MODULE := github.com/me/metaai2api
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

BIN := ./metaai2api
CMD := ./cmd/metaai2api

# WASM is where `make fetch-wasm` puts the codec module and what the
# wasm_codec build embeds.
WASM := internal/meta/dgw/codec/dgwcppbridge.wasm

.PHONY: build build-wasm test vet fmt check wasmcheck fetch-wasm clean-wasm clean

# The default build has no WASM codec and no OTA asset dependencies.
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

# A wasm-enabled build. It fails here, not later at runtime, when the module
# is missing, so a release never advertises a codec it does not carry.
build-wasm: require-wasm
	$(GO) build -trimpath -tags wasm_codec -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD)

require-wasm:
	@test -f "$(WASM)" || { \
	  echo "error: $(WASM) is missing; the wasm_codec build needs it." >&2; \
	  echo "       run 'make fetch-wasm SRC=<file-or-url>' first." >&2; \
	  exit 1; \
	}

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "error: files above need gofmt" >&2; exit 1; }
	$(GO) vet ./...
	$(GO) test ./...

# fetch-wasm installs the DGW codec module for the wasm_codec build.
#   make fetch-wasm SRC=/path/to/dgwcppbridge.wasm
#   make fetch-wasm SRC=https://host/dgwcppbridge.wasm
fetch-wasm:
	@test -n "$(SRC)" || { echo "usage: make fetch-wasm SRC=<file-or-url>" >&2; exit 2; }
	@case "$(SRC)" in \
	  http://*|https://*) curl -fsSL "$(SRC)" -o "$(WASM)" ;; \
	  *) cp "$(SRC)" "$(WASM)" ;; \
	esac
	@echo "installed $(WASM); build with: make build-wasm"

wasmcheck: require-wasm
	$(GO) build -tags wasm_codec -o ./wasmcheck ./cmd/wasmcheck
	./wasmcheck

clean-wasm:
	rm -f "$(WASM)" ./wasmcheck

clean: clean-wasm
	rm -f $(BIN)
