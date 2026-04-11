.PHONY: all build run exec

all: build exec

build:
	go build -o ./bin/vm ./cmd/vm/main.go
	codesign --entitlements entitlements.plist -f -s - ./bin/vm

run: build exec

exec:
	./bin/vm
