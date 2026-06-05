# Digitorn Go - Makefile
# Cross-platform Windows/Linux/macOS

BINARY_DAEMON := digitornd
BINARY_CLI    := digitorn
PKG           := github.com/mbathepaul/digitorn
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DATE    := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS       := -s -w \
                 -X $(PKG)/internal/version.Version=$(VERSION) \
                 -X $(PKG)/internal/version.BuildDate=$(BUILD_DATE)

GOFLAGS       := -trimpath
GO            := go

# Detect OS for binary extension
ifeq ($(OS),Windows_NT)
	EXT := .exe
else
	EXT :=
endif

.PHONY: all
all: tidy build

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: vendor
vendor:
	$(GO) mod vendor

WORKERS := digitorn-worker digitorn-worker-llm digitorn-worker-embeddings
DIST    := dist/digitorn-$(VERSION)

.PHONY: build
build: build-daemon build-cli build-workers

.PHONY: build-daemon
build-daemon:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY_DAEMON)$(EXT) ./cmd/digitornd

.PHONY: build-cli
build-cli:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY_CLI)$(EXT) ./cmd/digitorn

.PHONY: build-workers
build-workers:
	@for w in $(WORKERS); do \
		$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$$w$(EXT) ./cmd/$$w || exit 1; \
	done

# dist bundles the daemon, CLI and every worker into one folder so the
# service can be installed from it — resolveWorkerBinary finds the workers
# alongside the daemon executable. Deploy the whole folder.
.PHONY: dist
dist: build
	rm -rf $(DIST)
	mkdir -p $(DIST)
	cp bin/$(BINARY_DAEMON)$(EXT) bin/$(BINARY_CLI)$(EXT) $(DIST)/
	for w in $(WORKERS); do cp bin/$$w$(EXT) $(DIST)/; done
	cp config.example.yaml README.md $(DIST)/
	@echo "Bundle ready: $(DIST)/ — deploy this whole folder"

.PHONY: run
run: build-daemon build-workers
	./bin/$(BINARY_DAEMON)$(EXT)

.PHONY: test
test:
	$(GO) test -race -cover ./...

.PHONY: test-coverage
test-coverage:
	$(GO) test -race -coverprofile=coverage.txt -covermode=atomic ./...
	$(GO) tool cover -html=coverage.txt -o coverage.html

.PHONY: lint
lint:
	$(GO) vet ./...
	@which golangci-lint > /dev/null && golangci-lint run || echo "golangci-lint not installed"

.PHONY: clean
clean:
	rm -rf bin/ dist/ coverage.txt coverage.html

.PHONY: install-tools
install-tools:
	$(GO) install github.com/pressly/goose/v3/cmd/goose@latest
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

.PHONY: migrate-up
migrate-up:
	goose -dir migrations postgres "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down:
	goose -dir migrations postgres "$(DATABASE_URL)" down

.PHONY: docker-build
docker-build:
	docker build -t digitorn:$(VERSION) .

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  tidy           - Tidy go.mod"
	@echo "  build          - Build daemon + CLI + workers"
	@echo "  build-daemon   - Build daemon only"
	@echo "  build-cli      - Build CLI only"
	@echo "  build-workers  - Build worker binaries"
	@echo "  dist           - Bundle all binaries + config into dist/ for deployment"
	@echo "  run            - Build and run daemon"
	@echo "  test           - Run tests with race detector"
	@echo "  test-coverage  - Generate coverage report"
	@echo "  lint           - Run vet + golangci-lint"
	@echo "  clean          - Remove build artifacts"
	@echo "  migrate-up     - Apply DB migrations"
	@echo "  migrate-down   - Rollback last migration"
