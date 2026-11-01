.PHONY: build install uninstall test test-e2e test-e2e-native test-e2e-mixed test-e2e-launchd ci

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
#   go install github.com/tendant/qifa/cmd/qifa@latest
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/qifa
	@echo "installed -> $(GOBIN)/qifa"
	@command -v qifa >/dev/null 2>&1 || echo "warning: $(GOBIN) is not on PATH — add it: export PATH=\"$(GOBIN):\$$PATH\""

uninstall:
	rm -f $(GOBIN)/qifa

test:
	go test ./...

# Docker deploys against a throwaway SSH+Docker host, with a local zot
# registry and kamal-proxy.
test-e2e:
	test/e2e/docker-e2e.sh

# Native (systemd, no Docker on the target) deploys against a throwaway
# systemd host container. Needs Docker locally, privileged containers allowed.
test-e2e-native:
	test/e2e/native-e2e.sh

# Docker and native apps behind one (Docker) kamal-proxy on a systemd+Docker
# host.
test-e2e-mixed:
	test/e2e/mixed-e2e.sh

# Native deploys to this Mac as a LaunchAgent (native.init: launchd). macOS
# only; no Docker.
test-e2e-launchd:
	test/e2e/launchd-e2e.sh

ci: test test-e2e
