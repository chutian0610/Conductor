# Conductor server — Makefile
#
# Single entry point for every day-to-day command. CI should run
# `make verify` (or its components) and contributors should reach for
# `make help` first.
#
# Conventions:
#   - all Go targets use the system `go` (whatever `go version` reports).
#   - build output goes to bin/ at the server root (gitignored).
#   - coverage artefacts go to coverage.{out,html} (gitignored via *.out).

GO          ?= go
PKG         := ./...
BIN         := bin/conductor
COVER_OUT   := coverage.out
COVER_HTML  := coverage.html

# Default target: print help when invoked with no argument.
.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compile the CLI to bin/conductor.
	$(GO) build -o $(BIN) ./cmd/conductor

.PHONY: vet
vet: ## Run go vet across all packages.
	$(GO) vet $(PKG)

.PHONY: fmt
fmt: ## Show gofmt drift (no write). Wire into CI as: make fmt-check.
	@test -z "$$(gofmt -l . | tee /dev/stderr)"

.PHONY: test
test: ## Run unit + live-skipping tests (live tests auto-skip without CLI on PATH).
	$(GO) test $(PKG)

.PHONY: test-race
test-race: ## Run tests with -race (slow; useful before releases).
	$(GO) test -race $(PKG)

.PHONY: test-cover
test-cover: ## Run tests with coverage; emit coverage.out and coverage.html, print summary.
	$(GO) test -coverprofile=$(COVER_OUT) -covermode=atomic $(PKG)
	$(GO) tool cover -func=$(COVER_OUT) | tail -1
	$(GO) tool cover -html=$(COVER_OUT) -o $(COVER_HTML)
	@echo "open $(COVER_HTML) in a browser"

.PHONY: bench
bench: ## Run benchmarks (V1 has none yet — placeholder).
	$(GO) test -bench=. -benchmem $(PKG)

.PHONY: lint
lint: ## Run golangci-lint if installed (no-op otherwise). Config lands in a follow-up.
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not installed; skipping"; exit 0; }
	golangci-lint run ./...

.PHONY: verify
verify: fmt vet test build ## Pre-merge gate: vet + test + build (mirrors CI plan).
	@echo "verify OK"

.PHONY: run
run: build ## Run an example agent. Usage: make run CONFIG=examples/code-review-agent.yaml PROMPT="...".
	./$(BIN) run --config $${CONFIG:-examples/code-review-agent.yaml} --prompt "$${PROMPT:-Say hi in one line.}"

.PHONY: clean
clean: ## Remove build artefacts and coverage files.
	find . -name '*.test' -type f -delete 2>/dev/null || true
	find . -name '*.out' -type f -delete 2>/dev/null || true
	$(RM) $(COVER_OUT) $(COVER_HTML)
	$(RM) -rf bin

.PHONY: tidy
tidy: ## Run go mod tidy.
	$(GO) mod tidy
