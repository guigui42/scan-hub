# scan-hub developer shortcuts. `make` or `make help` lists the targets.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/scan-hub
IMAGE   ?= scan-hub:dev
# Playwright needs Node >= 20; prefer the newest nvm-installed version if any.
NODE_BIN := $(lastword $(sort $(wildcard $(HOME)/.nvm/versions/node/v2*/bin)))
NPM_REGISTRY := https://registry.npmjs.org/

.DEFAULT_GOAL := help
.PHONY: help build run demo test vet fmt e2e e2e-install docker-build docker-up docker-down clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/scan-hub
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/scan-hub

run: build ## Run against the real scanners with settings from .env (Ctrl-C releases the ScanSnap)
	@test -f .env || { echo "missing .env: cp .env.example .env and fill it in"; exit 1; }
	@set -a && . ./.env && set +a && echo "→ http://$${SCANHUB_LISTEN_ADDR:-127.0.0.1:8097}" && exec ./$(BIN)

demo: ## Run the UI against fake devices on http://127.0.0.1:8098 (no hardware)
	go run ./cmd/scan-hub-demo -addr 127.0.0.1:8098

test: ## Unit and integration tests (race detector)
	go test -race ./...

vet: ## go vet and gofmt check
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "run: make fmt"; exit 1; }

fmt: ## Format Go code
	gofmt -w cmd internal

e2e-install: ## Install Playwright and Chromium for the browser tests
	cd e2e && PATH="$(NODE_BIN):$$PATH" npm install --registry $(NPM_REGISTRY) --no-audit --no-fund
	cd e2e && PATH="$(NODE_BIN):$$PATH" npx playwright install chromium

e2e: ## Browser smoke tests against the fake-device demo
	cd e2e && PATH="$(NODE_BIN):$$PATH" npx playwright test

docker-build: ## Build the container image (scan-hub:dev)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-up: docker-build ## Run the image with compose.example.yaml and .env (real scanners)
	SCANHUB_IMAGE=$(IMAGE) docker compose --env-file .env -f compose.example.yaml up

docker-down: ## Stop the compose container (releases the ScanSnap)
	docker compose --env-file .env -f compose.example.yaml down

clean: ## Remove build outputs
	rm -rf bin e2e/test-results e2e/playwright-report
