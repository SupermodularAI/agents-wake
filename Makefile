# Development tasks. Requires nothing but the Go toolchain — golangci-lint is
# pinned in go.tools.mod and run through `go tool`, so `git clone && make` works
# on a machine with only Go installed.

GO      ?= go
BINARY  := wake
PKG     := github.com/SupermodularAI/agents-wake
DIST    := dist

# Dev tools live in a separate module file so their ~230 dependencies stay out of
# the main module's graph. See the comment at the top of go.tools.mod.
TOOLS   := -modfile=go.tools.mod
LINT    := $(GO) tool $(TOOLS) golangci-lint

# Build identity, resolved at invocation. internal/version's defaults exist only
# to make an un-injected binary obvious, so these must be passed.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

.DEFAULT_GOAL := help
.PHONY: help build run test vet lint fmt fmt-check validate tidy tools-update clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the static binary into dist/ (no cgo)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) ./cmd/$(BINARY)

run: build ## Build and run
	@./$(DIST)/$(BINARY)

test: ## Run unit tests
	$(GO) test ./...

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint
	$(LINT) run

fmt: ## Format the tree in place
	$(LINT) fmt

fmt-check: ## Fail if anything is unformatted
	$(LINT) fmt --diff

# The verify gate. AGENTS.md points at this target, and CI runs the same steps —
# one door in, so a green local run means a green CI run.
validate: fmt-check vet lint test ## The verify gate; must pass before a PR

tidy: ## Tidy the main module
	$(GO) mod tidy

tools-update: ## Re-pin golangci-lint (pass V=vX.Y.Z)
	@test -n '$(V)' || { echo 'usage: make tools-update V=v2.12.2'; exit 1; }
	$(GO) get $(TOOLS) -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(V)

clean: ## Remove build output
	rm -rf $(DIST)
