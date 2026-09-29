# Builds the Go program into build/app.wasm alongside the matching
# wasm_exec.js runtime glue. Select the compiler with GO=tinygo (default) or
# GO=go. app.wasm is rebuilt only when its inputs change.
GO ?= tinygo
PKG := ./cmd/hello
# The library's Go sources are inputs too: go.mod replaces it with ../..
GO_SOURCES := $(shell find cmd ../../durable ../../dosql -name '*.go' -not -name '*_test.go') go.mod

.PHONY: build clean
build: build/app.wasm

build/app.wasm: $(GO_SOURCES) build/compiler-$(GO)
ifeq ($(GO),tinygo)
	tinygo build -o build/app.wasm -target=wasm -no-debug -opt=z $(PKG)
	cp "$$(tinygo env TINYGOROOT)/targets/wasm_exec.js" build/wasm_exec.js
else ifeq ($(GO),go)
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o build/app.wasm $(PKG)
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" build/wasm_exec.js
else
	$(error GO must be "tinygo" or "go")
endif

# Records which compiler built app.wasm, so that switching GO rebuilds it.
build/compiler-$(GO):
	mkdir -p build
	rm -f build/compiler-*
	touch $@

clean:
	rm -rf build
