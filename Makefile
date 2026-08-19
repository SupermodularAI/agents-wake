# Development tasks. Requires nothing but the Go toolchain — golangci-lint is
# pinned in go.tools.mod and run through `go tool`, so `git clone && make` works
# on a machine with only Go installed.

GO      ?= go
BINARY  := wake
PKG     := github.com/SupermodularAI/agents-wake
DIST    := dist

# Same variable name and default as install.sh, so an already-exported
# WAKE_INSTALL_DIR picks the same place whichever install path someone uses.
WAKE_INSTALL_DIR ?= $(HOME)/.local/bin

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
.PHONY: help build install run test test-race vet lint fmt fmt-check validate tidy tools-update clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the static binary into dist/ (no cgo)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY) ./cmd/$(BINARY)

# Plain `install -m 0755`, not `go install`: the latter puts the binary in
# GOBIN or GOPATH/bin, a location most shells' PATH does not carry by
# default, which is exactly the "command not found" this target exists to
# avoid. WAKE_INSTALL_DIR is the same variable install.sh reads.
install: build ## Build and install the binary to WAKE_INSTALL_DIR (default ~/.local/bin)
	@mkdir -p $(WAKE_INSTALL_DIR)
	install -m 0755 $(DIST)/$(BINARY) $(WAKE_INSTALL_DIR)/$(BINARY)
	@echo "installed $(WAKE_INSTALL_DIR)/$(BINARY)"
	@case ":$$PATH:" in \
		*":$(WAKE_INSTALL_DIR):"*) ;; \
		*) echo "add $(WAKE_INSTALL_DIR) to PATH to run $(BINARY) without its full path" ;; \
	esac

run: build ## Build and run
	@./$(DIST)/$(BINARY)

test: ## Run unit tests
	$(GO) test ./...

# Deliberately outside `validate`: the race build roughly doubles a local verify
# loop, and the release gate names it as a check of its own alongside validate.
# CI runs it on every pull request.
test-race: ## Run unit tests with the race detector
	$(GO) test -race ./...

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
