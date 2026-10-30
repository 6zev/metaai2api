# Developer targets. The wasm module is not in this repository; see
# internal/meta/dgw/codec/testdata/README.md for why and how to supply it.

GO ?= go
WASM := internal/meta/dgw/codec/dgwcppbridge.wasm

.PHONY: build test wasmcheck fetch-wasm clean-wasm

build:
	$(GO) build ./...

test:
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
	@echo "installed $(WASM); build with -tags wasm_codec"

wasmcheck: fetch-wasm
	$(GO) build -tags wasm_codec -o ./wasmcheck ./cmd/wasmcheck
	./wasmcheck

clean-wasm:
	rm -f "$(WASM)" ./wasmcheck
