SHELL := /bin/bash

GO         := go
GOTOOLS    := gotools exec
GOLANGCI   := $(GOTOOLS) lint
ADDLICENSE := $(GOTOOLS) addlicense
TPARSE     := $(GOTOOLS) tparse
VULNCHECK  := $(GOTOOLS) vulncheck
MDLINT     := $(GOTOOLS) mdlint
YAMLFMT    := $(GOTOOLS) yamlfmt

LEFTHOOK  := $(GOTOOLS) lefthook

GOPKGS     := ./...

.DEFAULT_GOAL := help

.PHONY: help all build \
        fmt lint fix test cover bench \
        license tidy vuln mdlint yamlfmt \
        ci hooks-install hooks-run

help: ## show available targets
	@grep -E '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | \
		awk -F':.*## ' '{ printf "  %-22s %s\n", $$1, $$2 }'

# ── Development ─────────────────────────────────────────────────────────────

all: fix test ## fix then test

build: ## compile the package
	@$(GO) build $(GOPKGS)

fmt: yamlfmt ## format Go and YAML
	@$(GOLANGCI) fmt $(GOPKGS)

lint: ## run golangci-lint
	@$(GOLANGCI) run $(GOPKGS)

fix: ## go fix + golangci-lint --fix
	@$(GO) fix $(GOPKGS)
	@$(GOLANGCI) run --fix $(GOPKGS)

test: ## run tests with race detector and coverage
	@$(GO) test -race -json -coverpkg $(GOPKGS) -parallel=4 $(GOPKGS) | $(TPARSE) --all

cover: ## run tests with coverage profile
	@$(GO) test -race -count=1 -timeout 60s -coverprofile=coverage.out $(GOPKGS)
	@$(GO) tool cover -func=coverage.out

bench: ## run benchmarks
	@$(GO) test -bench=. -benchmem -count=3 $(GOPKGS)

tidy: ## tidy module dependencies
	@$(GO) mod tidy

# ── Formatting ──────────────────────────────────────────────────────────────

yamlfmt: ## format YAML files
	@$(YAMLFMT) .

license: ## add license headers to Go files
	@$(ADDLICENSE) -f LICENSE $$(find . -name "*.go" \
		-not -path "./tools/*" \
		-not -path "./_examples/*" \
		-not -path "./extras/*")

mdlint: ## lint markdown files
	@$(MDLINT) *.md

# ── Security ────────────────────────────────────────────────────────────────

vuln: ## run vulnerability check
	@$(VULNCHECK) $(GOPKGS)

# ── CI ──────────────────────────────────────────────────────────────────────

ci: build fmt lint test vuln mdlint ## run all checks

# ── Pre-commit ──────────────────────────────────────────────────────────────

hooks-install: ## install git hooks
	@$(LEFTHOOK) install

hooks-run: ## run pre-commit checks on staged files
	@$(LEFTHOOK) run pre-commit
