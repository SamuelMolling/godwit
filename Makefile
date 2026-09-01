.PHONY: all build test cover lint proto-lint tidy

all: lint proto-lint cover build

build:
	go build ./...

test:
	go test ./... -race

cover:
	./scripts/coverage.sh

lint:
	golangci-lint run

proto-lint:
	buf lint

tidy:
	go mod tidy
