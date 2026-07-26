# Agenda — build, test and run everything locally.
#
# There is exactly one build tool in this project: the Go compiler. No npm, no
# bundler, no code generation. `make check` is the gate everything must pass.

# Go may not be on PATH (it is installed under ~/.local/go for this machine).
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/go/bin/go)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DEVDATA := devdata

# Local development environment. Dev mode gives you: the /dev endpoints (mail sink,
# notification inbox, time travel), cookies without Secure so http://localhost works,
# and emails written to files instead of sent.
DEV_ENV := AGENDA_DEV=1 \
	AGENDA_LISTEN=127.0.0.1:8080 \
	AGENDA_DATA=$(DEVDATA)/agenda.db \
	AGENDA_TZ=Europe/Paris

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "Agenda — the family calendar"
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
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o agenda ./cmd/agenda
	@echo "built ./agenda ($(VERSION))"

.PHONY: build-all
build-all: ## Build release binaries for amd64 and arm64 (hardware changes over 20 years)
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agenda-$(VERSION)-linux-amd64 ./cmd/agenda
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/agenda-$(VERSION)-linux-arm64 ./cmd/agenda
	@ls -lh dist/

.PHONY: dev
dev: ## Run the server locally (http://localhost:8080)
	@mkdir -p $(DEVDATA)
	$(DEV_ENV) $(GO) run ./cmd/agenda serve

.PHONY: seed
seed: ## Reset the local database and fill it with a demo family
	@mkdir -p $(DEVDATA)
	@rm -f $(DEVDATA)/agenda.db $(DEVDATA)/agenda.db-wal $(DEVDATA)/agenda.db-shm
	@rm -rf $(DEVDATA)/mail
	$(DEV_ENV) $(GO) run ./cmd/agenda seed
	@echo
	@echo "Sign in at http://localhost:8080 with  maman@example.org / motdepasse"

.PHONY: backup
backup: ## Take a verified snapshot of the local database
	$(DEV_ENV) $(GO) run ./cmd/agenda backup $(DEVDATA)/backups

.PHONY: gen-vapid
gen-vapid: ## Generate a VAPID keypair (do this ONCE per deployment, then store it in Vault)
	$(GO) run ./cmd/agenda gen-vapid

.PHONY: vendor
vendor: ## Vendor and pin every dependency (run before tagging a release)
	$(GO) mod tidy
	$(GO) mod vendor
	@echo "vendored $$(ls vendor/ 2>/dev/null | wc -l) module roots"

.PHONY: e2e
e2e: ## Browser smoke tests (dev-only; needs npx playwright)
	@command -v npx >/dev/null 2>&1 || { echo "npx not installed — skipping browser tests"; exit 0; }
	cd e2e && npx playwright test

.PHONY: clean
clean: ## Remove build output and local dev state
	rm -rf agenda dist $(DEVDATA)
