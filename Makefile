.PHONY: all build install init proto run exec test test-e2e lint dist release clean

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

# Unit tests (-short skips the slow image conversions). VM tests are in e2e/.
test:
	@go test -short -count=1 ./...

# End-to-end tests: real VMs through the CLI, built with the hardened runtime
# like a release. RUN=TestDocker picks suites. See e2e/main_test.go.
test-e2e:
	@$(MAKE) build CODESIGN_FLAGS="--options runtime"
	@go test -tags e2e -count=1 -p 1 -v -timeout 75m $(if $(RUN),-run '$(RUN)') ./e2e/

lint:
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || { echo "gofmt needed:"; gofmt -l $$(git ls-files '*.go'); exit 1; }
	@go vet ./...
	@go vet -tags e2e ./e2e/
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

# Tag and push a release once lint, unit and e2e tests pass on this Mac:
# make release VERSION=v0.3.0. The Release workflow builds and publishes it.
release:
	@test "$(origin VERSION)" != file || { echo "usage: make release VERSION=vX.Y.Z[-rc.N]"; exit 1; }
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$$' || { echo "$(VERSION) isn't vX.Y.Z[-rc.N]"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "the working tree has uncommitted changes"; exit 1; }
	@test "$$(git branch --show-current)" = master || { echo "release from master"; exit 1; }
	@git fetch -q origin master
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/master)" || { echo "master isn't in sync with origin/master"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "tag $(VERSION) already exists"; exit 1; }
	@$(MAKE) lint test test-e2e
	@git tag -a "$(VERSION)" -m "$(VERSION)"
	@git push origin "$(VERSION)"

clean:
	@rm -rf bin $(DIST) $(INIT_BIN)
