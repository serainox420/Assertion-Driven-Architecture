# ADA — convenience targets. Each delegates to a script in scripts/ so the
# behavior is identical whether you use make or call the scripts directly.

.DEFAULT_GOAL := help
SHELL := /usr/bin/env bash

.PHONY: help deps build test race demo run model package clean fmt sync bench \
        up rebuild refresh shell stop start down

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

deps: ## Install dependencies (Go, jq, git, toolchain). Add WITH=--with-ollama etc.
	scripts/install.sh $(WITH)

build: ## Build the static ada_agent binary into bin/
	scripts/build.sh

test: ## gofmt check + go vet + go test
	scripts/test.sh

race: ## Run the test suite with the race detector
	scripts/test.sh --race

fmt: ## Format the Go sources in place
	gofmt -w ada cmd

demo: ## Run the offline demo (no model server needed)
	scripts/run.sh -demo

run: ## Run the agent. Pass args via ARGS="-objective '...' -model qwen2.5-coder:14b"
	scripts/run.sh $(ARGS)

model: ## Ensure Ollama is up and pull the worker model (override with MODEL=tag)
	scripts/model.sh $(MODEL)

package: ## Build the portable ada_toolkit tarball into dist/
	scripts/package.sh

sync: ## Update local files from a fresh clone (shows diffs, asks before writing). FLAGS="--dry-run"
	scripts/sync.sh $(FLAGS)

bench: ## Run benchmark objectives. LEVEL=L3 for one, empty for all
	scripts/bench.sh $(LEVEL)

clean: ## Remove build artifacts and benchmark results
	rm -rf bin dist benchmark/results

# ── Sandbox (isolated Docker container) ───────────────────────────────────────
# /ada is baked into the image, so the sandbox cannot touch the host tree. State
# (this container + the ada-home volume) persists across stop/start.

up: ## Start the sandbox (builds the image once if it doesn't exist; preserves state)
	docker compose up -d

rebuild: ## Rebuild the image from scratch and re-populate /ada from the working dir
	docker compose up -d --build

refresh: ## Re-populate /ada from the host working dir WITHOUT rebuilding the image
	scripts/sandbox-sync.sh

shell: ## Open a bash shell in the running sandbox
	docker compose exec arch bash

stop: ## Stop the sandbox, keeping all state (resume with `make start`)
	docker compose stop

start: ## Resume a stopped sandbox with its state intact
	docker compose start

down: ## Stop and remove the container (the ada-home volume is kept)
	docker compose down
