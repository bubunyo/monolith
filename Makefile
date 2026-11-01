.PHONY: all build install init proto run exec test test-vm lint dist clean

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

# monolith-init, PID 1 of container VMs, embedded in monolith (vm/guestinit).
INIT_BIN := vm/guestinit/files/monolith-init

# Regenerate the protobuf/ttRPC code for monolith-init's API
# (containerinit/agentpb). The generated files are committed; this needs only Go.
BUF_VERSION := v1.75.0
PROTOBUF_VERSION := $(shell go list -mod=mod -m -f '{{.Version}}' google.golang.org/protobuf)
TTRPC_VERSION := $(shell go list -mod=mod -m -f '{{.Version}}' github.com/containerd/ttrpc)
TOOLS := $(CURDIR)/bin/tools

proto:
	@GOBIN=$(TOOLS) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOBUF_VERSION)
	@GOBIN=$(TOOLS) go install github.com/containerd/ttrpc/cmd/protoc-gen-go-ttrpc@$(TTRPC_VERSION)
	@PATH="$(TOOLS):$$PATH" go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION) generate

init:
	@GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(INIT_BIN) ./cmd/monolith-init

build: init
	@go build -ldflags "$(LDFLAGS)" -o ./bin/monolith ./cmd/monolith/main.go
	@codesign --entitlements entitlements.plist -f $(CODESIGN_FLAGS) -s "$(CODESIGN_IDENTITY)" ./bin/monolith

install: init
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
	@GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go vet ./cmd/monolith-init ./containerinit

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
	@rm -rf bin $(DIST) $(INIT_BIN)
