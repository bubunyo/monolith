.PHONY: all build run exec

CODESIGN_IDENTITY ?= -

all: build exec

build:
	go build -o ./bin/monolith ./cmd/monolith/main.go
	codesign --entitlements entitlements.plist -f -s "$(CODESIGN_IDENTITY)" ./bin/monolith

run: build exec

exec:
	./bin/monolith vm run
