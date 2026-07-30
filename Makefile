SHELL := /bin/bash

GO         := go
GOTOOLS    := gotools exec
GOLANGCI   := $(GOTOOLS) lint
ADDLICENSE := $(GOTOOLS) addlicense
TPARSE     := $(GOTOOLS) tparse
VULNCHECK  := $(GOTOOLS) vulncheck
MDLINT     := $(GOTOOLS) mdlint
YAMLFMT    := $(GOTOOLS) yamlfmt
LEFTHOOK   := $(GOTOOLS) lefthook

GOPKGS     := ./...

.DEFAULT_GOAL := help

.PHONY: help all build fmt lint fix test cover bench tidy ci hooks

help: ## show available targets
	@grep -E '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | \
		awk -F':.*## ' '{ printf "  %-22s %s\n", $$1, $$2 }'

all: fix test ## fix then test

build: ## compile the package
	@$(GO) build $(GOPKGS)

fmt: ## format everything: tidy, license, YAML, Go
	@$(GO) mod tidy
	@$(ADDLICENSE) -f LICENSE $$(find . -name "*.go" -not -path "./tools/*")
	@$(YAMLFMT) .
	@$(GOLANGCI) fmt $(GOPKGS)

lint: ## run golangci-lint
	@$(GOLANGCI) run $(GOPKGS)

fix: ## auto-fix: go fix + lint fix + fmt
	@$(GO) fix $(GOPKGS)
	@$(GOLANGCI) run --fix $(GOPKGS)
	@$(MAKE) fmt

test: ## run tests with race and coverage
	@$(GO) test -race -json -coverpkg $(GOPKGS) -parallel=4 $(GOPKGS) | $(TPARSE) --all

cover: ## run tests with coverage profile
	@$(GO) test -race -count=1 -timeout 60s -coverprofile=coverage.out $(GOPKGS)
	@$(GO) tool cover -func=coverage.out

bench: ## run benchmarks
	@$(GO) test -bench=. -benchmem -count=3 $(GOPKGS)

tidy: ## tidy module dependencies
	@$(GO) mod tidy

ci: build fmt lint test ## run all checks
	@$(GO) vet $(GOPKGS)
	@$(VULNCHECK) $(GOPKGS)
	@$(MDLINT) *.md

hooks: ## install git hooks and run pre-commit
	@$(LEFTHOOK) install
	@$(LEFTHOOK) run pre-commit
