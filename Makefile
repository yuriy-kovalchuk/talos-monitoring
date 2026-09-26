BIN       := talos-monitoring
GO        ?= go
IMG       ?= talos-monitoring
TAG       ?= dev
K8S_VERSION ?= v1.36.1
CHART_DIR := charts/talos-monitoring
MODULE    := github.com/yuriy-kovalchuk/talos-monitoring

VERSION   ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILDDATE)

.PHONY: all build run test fmt vet lint tidy docker-build docker-push helm-lint helm-template

all: fmt vet build test lint

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/$(BIN)

run: build
	./bin/$(BIN) serve --listen :8080

test:
	$(GO) test -race ./...

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy

docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILDDATE=$(BUILDDATE) \
		-t $(IMG):$(TAG) .

docker-push:
	docker push $(IMG):$(TAG)

helm-lint:
	helm lint --strict $(CHART_DIR)

helm-template:
	helm template test-release $(CHART_DIR) --kube-version $(K8S_VERSION)
