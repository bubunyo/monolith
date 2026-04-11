.PHONY: all build run exec

all: build exec

build:
	go build -o ./bin/monolith ./cmd/monolith/main.go
	codesign --entitlements entitlements.plist -f -s - ./bin/monolith

run: build exec

exec:
	./bin/monolith vm run
