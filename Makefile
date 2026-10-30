.PHONY: all build install run exec test test-vm lint dist clean

# Read signing identity from .codesign_identity file if it exists,
# fall back to ad-hoc signing (-). Override with CODESIGN_IDENTITY env var.
CODESIGN_IDENTITY ?= $(shell cat .codesign_identity 2>/dev/null || echo -)
# Extra codesign flags; releases add --options runtime --timestamp for notarization.
CODESIGN_FLAGS ?=

# Version stamped into the binary (monolith --version).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/bubunyo/monolith/cli.version=$(VERSION)

DIST := dist
DIST_NAME := monolith_$(VERSION:v%=%)_darwin_arm64

all: build

build:
	@go build -ldflags "$(LDFLAGS)" -o ./bin/monolith ./cmd/monolith/main.go
	@codesign --entitlements entitlements.plist -f $(CODESIGN_FLAGS) -s "$(CODESIGN_IDENTITY)" ./bin/monolith

install:
	@go install -ldflags "$(LDFLAGS)" ./cmd/monolith
	@codesign --entitlements entitlements.plist -f $(CODESIGN_FLAGS) -s "$(CODESIGN_IDENTITY)" "$$(go env GOBIN 2>/dev/null | grep . || echo $$(go env GOPATH)/bin)/monolith"

run: build exec

exec:
	@./bin/monolith vm run

# Unit tests only: -short skips tests that boot VMs or create vmnet networks.
test:
	@go test -short -count=1 ./...

# Everything, including tests that boot real VMs (needs the signed binary).
test-vm: build
	@go test -count=1 ./...

lint:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { echo "gofmt needed:"; gofmt -l $$(git ls-files '*.go'); exit 1; }
	@go vet ./...

# Release archive (binary, LICENSE, README) and its checksum in dist/.
# Signs with the bin/monolith that build produced; run build first with the
# release VERSION and signing settings.
dist:
	@rm -rf $(DIST) && mkdir -p $(DIST)/$(DIST_NAME)
	@cp bin/monolith LICENSE README.md $(DIST)/$(DIST_NAME)/
	@tar -C $(DIST) -czf $(DIST)/$(DIST_NAME).tar.gz $(DIST_NAME)
	@rm -rf $(DIST)/$(DIST_NAME)
	@cd $(DIST) && shasum -a 256 $(DIST_NAME).tar.gz > checksums.txt
	@echo "$(DIST)/$(DIST_NAME).tar.gz"

clean:
	@rm -rf bin $(DIST)
