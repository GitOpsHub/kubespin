BINARY      := kubespin
PKG         := github.com/GitOpsHub/kubespin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT) \
               -X $(PKG)/internal/version.BuildDate=$(DATE)

GOLANGCI_VERSION := v2.6.2

## Where `install` puts the binary. ~/.local/bin is already on PATH on macOS
## and writable without sudo, unlike /usr/local/bin. Override for elsewhere:
##   make install INSTALL_DIR=/usr/local/bin
INSTALL_DIR ?= $(HOME)/.local/bin

.DEFAULT_GOAL := all

.PHONY: all
all: lint test build

## Also installs onto PATH, so `kubespin ...` works from any directory
## without a ./bin/ prefix.
##
## Skipped when CI is set: a runner has no use for it, and writing outside the
## repo tree is a surprising side effect for a build to have. Use `make build
## INSTALL_DIR=...` to redirect it, or `make lambda` plus a bare `go build` to
## avoid it entirely.
.PHONY: build
build: lambda
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)
ifndef CI
	@$(MAKE) --no-print-directory install
endif

## Copies the built binary onto PATH. Kept separate from build so it can be
## re-run (or pointed somewhere else) without a rebuild.
.PHONY: install
install:
	@test -x bin/$(BINARY) || { echo "bin/$(BINARY) is not built; run 'make build'" >&2; exit 1; }
	@mkdir -p '$(INSTALL_DIR)'
	@install -m 0755 bin/$(BINARY) '$(INSTALL_DIR)/$(BINARY)'
	@printf '==> installed %s %s to %s\n' '$(BINARY)' '$(VERSION)' '$(INSTALL_DIR)/$(BINARY)'

## The ingestion Lambda runs on provided.al2023, where the handler must be a
## Linux binary named `bootstrap`. GOOS/GOARCH are set inline so a cross-compile
## of the main binary cannot leak into this target.
.PHONY: lambda
lambda:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags '-s -w $(LDFLAGS)' -o bin/ingestion/bootstrap ./cmd/ingestion

.PHONY: test
test:
	go test -race -cover ./...

## Integration tests need real cloud credentials and a reachable Postgres
## (KUBESPIN_POSTGRES_TEST_DSN); opt-in only.
.PHONY: integration
integration:
	go test -race -tags=integration ./...

.PHONY: lint
lint:
	golangci-lint run

## Regenerates docs/cli from the command tree. Must be a no-op when current.
.PHONY: docs
docs:
	go run ./internal/tools/docsgen

.PHONY: fmt
fmt:
	go fmt ./...
	go mod tidy

## Installs developer tooling that isn't vendored through go.mod.
.PHONY: bootstrap
bootstrap:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

.PHONY: clean
clean:
	rm -rf bin/
