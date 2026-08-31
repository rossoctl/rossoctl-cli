BINARY       := rossoctl
PKG          := github.com/rossoctl/rossoctl-cli
CMD_PKG      := $(PKG)/cmd
BUILDINFO_PKG := $(PKG)/internal/buildinfo

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS  := -s -w \
	-X '$(BUILDINFO_PKG).Version=$(VERSION)' \
	-X '$(CMD_PKG).commit=$(COMMIT)' \
	-X '$(CMD_PKG).date=$(DATE)'

.PHONY: all build install test lint fmt vet tidy clean

all: build

build: ## Build the binary into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

install: ## Install the binary into $GOBIN
	go install -ldflags "$(LDFLAGS)" .

test: ## Run tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format the code
	go fmt ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin
