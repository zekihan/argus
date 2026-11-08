GO ?= go
HELM ?= helm
PYTHON ?= python3
VERSION := $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fmt lint test test/race test/integration audit container-test chart-test chart-package chart-released-check release-check
build:
	mkdir -p dist
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/argus ./cmd/argus
fmt:
	gofmt -w cmd internal tests/integration
lint:
	@test -z "$$(gofmt -l cmd internal tests/integration)" || (gofmt -l cmd internal tests/integration; exit 1)
	golangci-lint run --build-tags=integration
test:
	$(GO) test -count=1 ./...
test/race:
	$(GO) test -race -count=1 ./...
test/integration: build
	ARGUS_BINARY=$(CURDIR)/dist/argus $(GO) test -count=1 -tags=integration ./tests/integration
audit: lint test test/race test/integration
	$(GO) mod verify
container-test:
	./scripts/container_smoke.sh
chart-test: build
	$(HELM) lint --strict charts/argus -f charts/argus/ci/default-values.yaml
	$(HELM) lint --strict charts/argus -f charts/argus/ci/all-checks-values.yaml
	$(HELM) lint --strict charts/argus -f charts/argus/ci/full-config-values.yaml
	HELM=$(HELM) $(PYTHON) tests/helm/test_chart.py
chart-package: chart-test
	mkdir -p dist/charts
	$(HELM) package charts/argus --destination dist/charts
chart-released-check: build
	HELM=$(HELM) PYTHON=$(PYTHON) ./scripts/check_released_chart.sh dist/argus
release-check:
	./scripts/release_version.sh
	goreleaser check
