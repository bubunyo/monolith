.PHONY: all
all: build run

build:
	go build -o ./bin/vm ./cmd/vm/main.go
	codesign --entitlements entitlements.plist -f -s - ./bin/vm

run:
	./bin/vm
