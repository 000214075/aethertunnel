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
CLIENT_LDFLAGS := -s -w \
            -X github.com/aethertunnel/aethertunnel/pkg/clientlib.Version=$(VERSION) \
            -X github.com/aethertunnel/aethertunnel/pkg/clientlib.BuildTime=$(DATE) \
            -X github.com/aethertunnel/aethertunnel/pkg/clientlib.GitCommit=$(COMMIT)

GO       ?= go
GOFMT    ?= $(shell $(GO) env GOROOT)/bin/gofmt
GOFLAGS  :=

.PHONY: all build test test-race vet fmt lint tidy clean cross check run-server run-client

all: fmt vet test build

## build: native binaries into bin/
build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINDIR)/aethertunnel-server .
	$(GO) build $(GOFLAGS) -ldflags "$(CLIENT_LDFLAGS)" -o $(BINDIR)/aethertunnel-client ./client

## test: unit and integration tests (the tunnel test binds loopback ports)
test:
	$(GO) test ./... -count=1 -timeout 300s

## test-race: the same tests under the race detector. That needs cgo and a C
## compiler; where there is none, scripts/race-toolchain.sh unpacks one into a cache
## directory without root (set CC to use another).
test-race:
	CC="$${CC:-$$(bash scripts/race-toolchain.sh)}" CGO_ENABLED=1 $(GO) test ./... -count=1 -race -timeout 900s

## vet: static checks
vet:
	$(GO) vet ./...

## lint: report files that are not gofmt-ed (make fmt writes them), then vet.
## This is the same pair of checks the Linux CI job runs before the tests.
lint:
	@if [ ! -x "$(GOFMT)" ]; then echo "no gofmt at $(GOFMT); set GOFMT="; exit 1; fi
	@unformatted="$$($(GOFMT) -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt-ed; run 'make fmt':"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	$(GO) vet ./...

## fmt: format every Go file
fmt:
	$(GO) fmt ./...

## tidy: synchronise go.mod and go.sum
tidy:
	$(GO) mod tidy

## check: validate the example configurations through the binaries. The strict flag
## makes a key this version does not understand an error rather than a warning, which
## is what turns the check into a guard against a typo in the examples themselves.
check: build
	$(BINDIR)/aethertunnel-server --config server.toml.example --check --reject-unknown-keys
	$(BINDIR)/aethertunnel-client --config client.toml.example --check --reject-unknown-keys

## cross: build the release matrix into dist/ with checksums.
## Invoked through bash on purpose: a checkout that lost the executable bit (a zip
## download, or a git whose core.fileMode is off) would otherwise fail here.
cross:
	bash scripts/build-release.sh

## clean: remove build output
clean:
	rm -rf $(BINDIR) dist

## run-server: run the server with the example configuration
run-server: build
	$(BINDIR)/aethertunnel-server --config server.toml.example

## run-client: run the client with the example configuration
run-client: build
	$(BINDIR)/aethertunnel-client --config client.toml.example
