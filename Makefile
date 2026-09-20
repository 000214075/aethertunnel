# AetherTunnel — build and test entry points.
#
# On Windows without make, use scripts\build-release.ps1 for the same cross-build.

MODULE   := github.com/aethertunnel/aethertunnel
BINDIR   := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
            -X main.version=$(VERSION) \
            -X main.buildTime=$(DATE) \
            -X main.gitCommit=$(COMMIT)

GO       ?= go
GOFLAGS  :=

.PHONY: all build test vet fmt lint tidy clean cross release check run-server run-client

all: fmt vet test build

## build: native binaries into bin/
build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINDIR)/aethertunnel-server .
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINDIR)/aethertunnel-client ./client

## test: unit and integration tests (the tunnel test binds loopback ports)
test:
	$(GO) test ./... -count=1 -timeout 180s

## vet: static checks
vet:
	$(GO) vet ./...

## fmt: format every Go file
fmt:
	$(GO) fmt ./...

## tidy: synchronise go.mod and go.sum
tidy:
	$(GO) mod tidy

## check: validate the example configurations through the binaries
check: build
	$(BINDIR)/aethertunnel-server --config server.toml.example --check
	$(BINDIR)/aethertunnel-client --config client.toml.example --check

## cross: build the release matrix into dist/ with checksums
cross:
	./scripts/build-release.sh

## clean: remove build output
clean:
	rm -rf $(BINDIR) dist

## run-server: run the server with the example configuration
run-server: build
	$(BINDIR)/aethertunnel-server --config server.toml.example

## run-client: run the client with the example configuration
run-client: build
	$(BINDIR)/aethertunnel-client --config client.toml.example
