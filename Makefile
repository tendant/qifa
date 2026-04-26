.PHONY: build install uninstall test test-e2e ci

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Resolve the install destination the same way `go install` does: GOBIN if
# set, otherwise $(go env GOPATH)/bin. Printed by `make install` so users
# know what to add to PATH.
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif

build:
	go build -ldflags "$(LDFLAGS)" ./cmd/qifa

# Install from local source into $GOBIN (or $GOPATH/bin). Version metadata is
# stamped via -ldflags so `qifa version` reflects the working tree.
#
# To install a published release without cloning:
#   go install github.com/gokamal/gocart/cmd/qifa@latest
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/qifa
	@echo "installed -> $(GOBIN)/qifa"
	@command -v qifa >/dev/null 2>&1 || echo "warning: $(GOBIN) is not on PATH — add it: export PATH=\"$(GOBIN):\$$PATH\""

uninstall:
	rm -f $(GOBIN)/qifa

test:
	go test ./...

test-e2e:
	bash scripts/test-zot-e2e.sh

ci: test test-e2e
