.PHONY: build dist install uninstall test test-install test-e2e test-e2e-native test-e2e-mixed test-e2e-launchd test-e2e-vps ci

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

# Release binaries: static (no cgo) builds for each platform in PLATFORMS,
# named qifa-<os>-<arch> so install.sh can pick one without knowing the
# version, plus SHA256SUMS over all of them. The release workflow runs this
# with VERSION set to the tag.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

dist:
	mkdir -p dist
	rm -f dist/qifa-* dist/SHA256SUMS
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building dist/qifa-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w $(LDFLAGS)" -o dist/qifa-$$os-$$arch ./cmd/qifa || exit 1; \
	done
	cd dist && { command -v sha256sum >/dev/null && sha256sum qifa-* || shasum -a 256 qifa-*; } > SHA256SUMS
	@cat dist/SHA256SUMS

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

# install.sh against freshly built dist/ binaries served from disk: a good
# release installs, a checksum mismatch or a missing entry installs nothing.
test-install: dist
	test/install-test.sh

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

# docs/vps.md, word for word: a fresh Ubuntu server (a container) to the app
# over HTTPS, with Pebble issuing the certificate. Needs the internet.
test-e2e-vps:
	test/e2e/vps-e2e.sh

# Native deploys to this Mac as a LaunchAgent (native.init: launchd). macOS
# only; no Docker.
test-e2e-launchd:
	test/e2e/launchd-e2e.sh

ci: test test-e2e
