.PHONY: all build test cover e2e load chaos lint proto-lint tidy helm-lint release-snapshot

all: lint proto-lint cover build

build:
	go build ./...

test:
	go test ./... -race

cover:
	./scripts/coverage.sh

e2e:
	go test -tags e2e -count=1 -timeout 15m ./test/e2e/...

# Slow and deliberately outside `all`: see docs/testing.md for the knobs and the numbers.
load:
	go test -tags load -count=1 -timeout 120m -v ./test/e2e/...

chaos:
	go test -tags chaos -count=1 -timeout 60m -v ./test/e2e/...

lint:
	golangci-lint run

proto-lint:
	buf lint

tidy:
	go mod tidy

HELM_CHART := deploy/helm/godwit

helm-lint:
	helm lint $(HELM_CHART)
	helm lint $(HELM_CHART) -f $(HELM_CHART)/ci/full-values.yaml
	helm template godwit $(HELM_CHART) > /dev/null
	helm template godwit $(HELM_CHART) -f $(HELM_CHART)/ci/full-values.yaml > /dev/null

release-snapshot:
	goreleaser release --snapshot --clean --skip=publish
