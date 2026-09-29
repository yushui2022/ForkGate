GO ?= go
GOLANGCI_LINT ?= golangci-lint
BINARY ?= bin/forkgate

# Keep Go's growing caches and temporary build files off the system drive.
# Override FORKGATE_CACHE_ROOT on the make command line when needed.
FORKGATE_CACHE_ROOT ?= G:/DevCache
GOPATH = $(FORKGATE_CACHE_ROOT)/gopath
GOMODCACHE = $(FORKGATE_CACHE_ROOT)/go-mod
GOCACHE = $(FORKGATE_CACHE_ROOT)/go-build
GOTMPDIR = $(FORKGATE_CACHE_ROOT)/Temp/ForkGate/go
TEMP = $(FORKGATE_CACHE_ROOT)/Temp/ForkGate/go
TMP = $(FORKGATE_CACHE_ROOT)/Temp/ForkGate/go
export GOPATH GOMODCACHE GOCACHE GOTMPDIR TEMP TMP

.PHONY: all build test vet lint check

all: check build

build:
	$(GO) build -trimpath -o $(BINARY) ./cmd/forkgate

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

check: test vet lint
