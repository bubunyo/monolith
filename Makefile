.PHONY: all build install run exec

# Read signing identity from .codesign_identity file if it exists,
# fall back to ad-hoc signing (-). Override with CODESIGN_IDENTITY env var.
CODESIGN_IDENTITY ?= $(shell cat .codesign_identity 2>/dev/null || echo -)

all: build

build:
	@go build -o ./bin/monolith ./cmd/monolith/main.go
	@codesign --entitlements entitlements.plist -f -s "$(CODESIGN_IDENTITY)" ./bin/monolith

install:
	@go install ./cmd/monolith
	@codesign --entitlements entitlements.plist -f -s "$(CODESIGN_IDENTITY)" "$$(go env GOBIN 2>/dev/null | grep . || echo $$(go env GOPATH)/bin)/monolith"

run: build exec

exec:
	@./bin/monolith vm run
