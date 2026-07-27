# Almanack — build, test and run everything locally.
#
# There is exactly one build tool in this project: the Go compiler. No npm, no
# bundler, no code generation. `make check` is the gate everything must pass.

# Go may not be on PATH (it is installed under ~/.local/go for this machine).
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)

ifeq ($(wildcard $(GO)),)
ifeq ($(shell command -v $(GO) 2>/dev/null),)
$(error Go 1.25+ is required and was not found. Install it from https://go.dev/dl/ \
or set GO=/path/to/go)
endif
endif

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DEVDATA := devdata

# The platforms a release ships, as GOOS/name. Linux is where this actually runs — a
# VPS, a mini PC, a Raspberry Pi — and armv7 is there because a Pi 3 or a Zero 2 W on
# 32-bit Raspberry Pi OS is exactly the cheap box in a cupboard this is for. macOS is
# for trying it on a laptop before committing a box to it.
#
# CGO is off and modernc.org/sqlite is pure Go, so every one of these cross-compiles
# from any of them: `make build-all` produces the whole set on a developer's machine
# and in CI alike, and the release workflow runs this exact target.
RELEASE_TARGETS := linux/amd64 linux/arm64 linux/armv7 darwin/amd64 darwin/arm64

# sha256sum on Linux, shasum on macOS. Same output format either way.
SHASUM := $(shell command -v sha256sum 2>/dev/null || echo "shasum -a 256")

# Local development environment. Dev mode gives you: the /dev endpoints (mail sink,
# notification inbox, time travel), cookies without Secure so http://localhost works,
# and emails written to files instead of sent.
DEV_ENV := ALMANACK_DEV=1 \
	ALMANACK_LISTEN=127.0.0.1:8080 \
	ALMANACK_DATA=$(DEVDATA)/almanack.db \
	ALMANACK_TZ=Europe/Paris

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "Almanack — a shared calendar with a ten-year shelf life"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Start here:  make seed && make dev  →  http://localhost:8080"

.PHONY: check
check: fmt-check vet test ## Everything that must pass before code is done

.PHONY: test
test: ## Run all tests
	$(GO) test ./...

.PHONY: test-v
test-v: ## Run all tests, verbose
	$(GO) test -v ./...

.PHONY: cover
cover: ## Run tests with a coverage summary
	@mkdir -p $(DEVDATA)
	$(GO) test -coverprofile=$(DEVDATA)/coverage.out ./... && \
	$(GO) tool cover -func=$(DEVDATA)/coverage.out | tail -30

.PHONY: race
race: ## Run tests with the race detector (the scheduler shares state with HTTP handlers)
	$(GO) test -race ./...

.PHONY: fmt
fmt: ## Format all Go code
	$(GO) fmt ./...

GOFMT ?= $(dir $(GO))gofmt

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-clean
	@out=$$($(GOFMT) -l . 2>/dev/null | grep -v '^vendor/' || true); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: build
build: ## Build the static binary for this machine
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o almanack ./cmd/almanack
	@echo "built ./almanack ($(VERSION))"

.PHONY: build-all
build-all: ## Build every release binary, plus SHA256SUMS, into dist/
	@rm -rf dist && mkdir -p dist
	@for t in $(RELEASE_TARGETS); do \
		os=$${t%/*}; name=$${t#*/}; arch=$$name; goarm=; \
		if [ "$$name" = "armv7" ]; then arch=arm; goarm=7; fi; \
		out=dist/almanack-$(VERSION)-$$os-$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$goarm \
			$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/almanack || exit 1; \
		echo "built $$out"; \
	done
	@cd dist && $(SHASUM) almanack-* > SHA256SUMS
	@echo
	@ls -lh dist/

.PHONY: dev
dev: ## Run the server locally (http://localhost:8080)
	@mkdir -p $(DEVDATA)
	$(DEV_ENV) $(GO) run ./cmd/almanack serve

.PHONY: seed
seed: ## Reset the local database and fill it with a demo family
	@mkdir -p $(DEVDATA)
	@rm -f $(DEVDATA)/almanack.db $(DEVDATA)/almanack.db-wal $(DEVDATA)/almanack.db-shm
	@rm -rf $(DEVDATA)/mail
	$(DEV_ENV) $(GO) run ./cmd/almanack seed
	@echo
	@echo "Sign in at http://localhost:8080 with  mum@example.org / password"

.PHONY: backup
backup: ## Take a verified snapshot of the local database
	$(DEV_ENV) $(GO) run ./cmd/almanack backup $(DEVDATA)/backups

.PHONY: gen-vapid
gen-vapid: ## Generate a VAPID keypair (do this ONCE per deployment, then store it in Vault)
	$(GO) run ./cmd/almanack gen-vapid

.PHONY: vendor
vendor: ## Vendor and pin every dependency (run before tagging a release)
	$(GO) mod tidy
	$(GO) mod vendor
	@echo "vendored $$(ls vendor/ 2>/dev/null | wc -l) module roots"

.PHONY: e2e
e2e: ## Browser smoke tests (dev-only; needs npx playwright)
	@if command -v npx >/dev/null 2>&1; then \
		cd e2e && npx playwright test; \
	else \
		echo "npx not installed — skipping browser tests"; \
	fi

.PHONY: clean
clean: ## Remove build output and local dev state
	rm -rf almanack dist $(DEVDATA)
