.PHONY: all build test cover e2e lint proto-lint tidy helm-lint

all: lint proto-lint cover build

build:
	go build ./...

test:
	go test ./... -race

cover:
	./scripts/coverage.sh

e2e:
	go test -tags e2e -count=1 -timeout 15m ./test/e2e/...

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
