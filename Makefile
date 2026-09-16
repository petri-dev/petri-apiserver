IMG ?= petri-apiserver:latest

CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build
.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format Go source, including E2E tests.
	gofmt -w cmd internal test

.PHONY: check-fmt
check-fmt: ## Check formatting without changing files.
	@files="$$(gofmt -l cmd internal test)"; test -z "$$files" || { printf '%s\n' "$$files"; exit 1; }

.PHONY: vet
vet: ## Run go vet, including E2E tests.
	go vet -tags=e2e ./...

.PHONY: test
test: check-fmt vet ## Run race tests and write coverage to cover.out.
	go test -race ./... -coverprofile cover.out

.PHONY: lint
lint: ## Run strict lint checks.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

.PHONY: lint-fix
lint-fix: ## Apply automatic lint fixes.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --fix

.PHONY: build
build: check-fmt vet ## Build server binary.
	go build -o bin/server ./cmd/server

.PHONY: run
run: ## Run the server with the current environment and kubeconfig.
	go run ./cmd/server

.PHONY: docker-build
docker-build: ## Build a local container image (IMG).
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push IMG to its registry.
	$(CONTAINER_TOOL) push ${IMG}

PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
docker-buildx: ## Build and push IMG for PLATFORMS.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} .

KIND_CLUSTER ?=
KIND ?= kind

.PHONY: test-e2e
test-e2e: ## Build and test the real lifecycle in a new disposable Kind cluster.
	KIND="$(KIND)" KIND_CLUSTER="$(KIND_CLUSTER)" \
		go test -tags=e2e ./test/e2e/ -count=1 -v -timeout 30m

.PHONY: deploy
deploy: ## Apply the deployment example to the current Kubernetes context.
	kubectl apply -k config/

.PHONY: undeploy
undeploy: ## Delete example resources from the current Kubernetes context.
	kubectl delete -k config/ --ignore-not-found=true

GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: validate-manifests
validate-manifests: ## Render every shipped Kustomize entry point without cluster access.
	@for path in config config/rbac test/e2e/testdata/apiserver; do kubectl kustomize "$$path" > /dev/null; done
