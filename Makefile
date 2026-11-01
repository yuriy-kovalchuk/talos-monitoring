BIN       := talos-monitoring
GO        ?= go
IMG       ?= talos-monitoring
TAG       ?= dev
K8S_VERSION ?= v1.37.1
CHART_DIR := charts/talos-monitoring
MODULE    := github.com/yuriy-kovalchuk/talos-monitoring

VERSION   ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILDDATE)

.PHONY: all build run test fmt vet lint tidy docker-build docker-push helm-lint helm-template install-hooks

all: fmt vet build test lint

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/$(BIN)

# make run [TALOSCONFIG=path] [KUBECONFIG=path] [LISTEN=:8080]
# Unset paths fall back to $TALOSCONFIG/~/.talos/config and $KUBECONFIG/~/.kube/config;
# the recipe shell expands a leading ~, so TALOSCONFIG=~/.talos/config-prod works.
LISTEN ?= :8080
run: build
	$(if $(TALOSCONFIG),TALOSCONFIG=$(TALOSCONFIG) )$(if $(KUBECONFIG),KUBECONFIG=$(KUBECONFIG) )./bin/$(BIN) serve --listen $(LISTEN)

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

# Points git at .githooks/: commit-msg checks each subject, pre-push re-checks pushes to main, tags and tests.
install-hooks:
	git config core.hooksPath .githooks && chmod +x .githooks/*
