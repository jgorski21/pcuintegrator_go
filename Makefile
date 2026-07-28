SHELL := /bin/bash
GO_IMAGE ?= golang:1.26-alpine
GO_DOCKER = docker run --rm -v "$(CURDIR)/container_src":/src -w /src \
	-v pcu-gocache:/root/.cache/go-build $(GO_IMAGE)
IMAGE ?= pcu:local

.DEFAULT_GOAL := help
.PHONY: help test fmt vet build run dry-run status dev deploy tail types typecheck clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## Runs with a local Go toolchain when there is one (`brew install go`), otherwise
## inside the same image the container is built from. Tests are architecture
## independent, so they run natively either way.
test: ## Run the Go test suite
	@if command -v go >/dev/null 2>&1; then \
		cd container_src && gofmt -l . && go vet ./... && go test ./... ; \
	else \
		echo "no local go; running in $(GO_IMAGE)"; \
		$(GO_DOCKER) sh -c 'gofmt -l . && go vet ./... && go test ./...' ; \
	fi

fmt: ## Format the Go sources
	@if command -v go >/dev/null 2>&1; then cd container_src && gofmt -w . ; \
	else $(GO_DOCKER) gofmt -w . ; fi

vet: ## go vet only
	@if command -v go >/dev/null 2>&1; then cd container_src && go vet ./... ; \
	else $(GO_DOCKER) go vet ./... ; fi

build: ## Build the linux/amd64 container image
	docker build -t $(IMAGE) ./container_src
	@echo -n "image architecture (must be linux/amd64): "
	@docker image inspect $(IMAGE) --format '{{.Os}}/{{.Architecture}}'

## The image is amd64 and this host is arm64, so Docker runs it under emulation.
## It is the exact artifact that ships — slightly slower to start, identical
## behaviour.
run: build ## Run the container locally on :8080 using .dev.vars
	docker run --rm --platform linux/amd64 -p 8080:8080 --env-file .dev.vars $(IMAGE)

dry-run: ## Hit the locally running container read-only (needs `make run` in another shell)
	curl -s 'http://127.0.0.1:8080/sync?dry_run=true&explain=true' | jq .

status: ## Show the locally running container's run status
	curl -s http://127.0.0.1:8080/status | jq .

dev: ## wrangler dev (builds and runs the container through Docker)
	npx wrangler dev

deploy: test ## Deploy the Worker and push the container image
	npx wrangler deploy

tail: ## Live Worker + container logs
	npx wrangler tail --format pretty

types: ## Regenerate worker-configuration.d.ts from wrangler.jsonc
	npx wrangler types

typecheck: types ## Type-check the Worker
	npx tsc --noEmit

clean: ## Remove local build artefacts
	-docker image rm $(IMAGE)
	-docker volume rm pcu-gocache
	rm -rf node_modules .wrangler
