# Digitorn Go - Makefile
# Local dev: build for the current platform only.
# Cross-platform releases: use GoReleaser (goreleaser release).

BINARY_DAEMON := digitornd
BINARY_CLI    := digitorn
PKG           := github.com/digitornai/digitorn
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DATE    := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS       := -s -w \
                 -X $(PKG)/internal/version.Version=$(VERSION) \
                 -X $(PKG)/internal/version.BuildDate=$(BUILD_DATE)

GOFLAGS       := -trimpath
GO            := go

ifeq ($(OS),Windows_NT)
	EXT := .exe
else
	EXT :=
endif

WORKERS  := digitorn-worker digitorn-worker-llm digitorn-worker-embeddings digitorn-worker-tokenizer
SERVICES := digitorn-background digitorn-voice
ALL_BINS := $(BINARY_DAEMON) $(BINARY_CLI) $(WORKERS) $(SERVICES)
DIST     := dist/digitorn-$(VERSION)

.PHONY: all
all: tidy build

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: vendor
vendor:
	$(GO) mod vendor

.PHONY: build
build: build-daemon build-cli build-workers build-services

.PHONY: build-daemon
build-daemon:
	$(GO) build $(GOFLAGS) -tags treesitter -ldflags "$(LDFLAGS)" -o bin/$(BINARY_DAEMON)$(EXT) ./cmd/digitornd

.PHONY: build-cli
build-cli:
	$(GO) build $(GOFLAGS) -ldflags "-s -w" -o bin/$(BINARY_CLI)$(EXT) ./cmd/digitorn

.PHONY: build-workers
build-workers:
	@for w in $(WORKERS); do \
		$(GO) build $(GOFLAGS) -tags treesitter -ldflags "$(LDFLAGS)" -o bin/$$w$(EXT) ./cmd/$$w || exit 1; \
	done

.PHONY: build-services
build-services:
	@for s in $(SERVICES); do \
		$(GO) build $(GOFLAGS) -tags treesitter -ldflags "$(LDFLAGS)" -o bin/$$s$(EXT) ./cmd/$$s || exit 1; \
	done

.PHONY: dist
dist: build
	rm -rf $(DIST)
	mkdir -p $(DIST)
	@for b in $(ALL_BINS); do cp bin/$$b$(EXT) $(DIST)/; done
	cp config.example.yaml README.md LICENSE $(DIST)/
	@echo "Bundle ready: $(DIST)/ — deploy this whole folder"

.PHONY: run
run: build-daemon build-workers build-services
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
	@echo "  build          - Build daemon + CLI + workers + services (current platform)"
	@echo "  build-daemon   - Build daemon only"
	@echo "  build-cli      - Build CLI only"
	@echo "  build-workers  - Build worker binaries"
	@echo "  build-services - Build background + voice services"
	@echo "  dist           - Bundle all binaries + config into dist/ for deployment"
	@echo "  run            - Build and run daemon"
	@echo "  test           - Run tests with race detector"
	@echo "  test-coverage  - Generate coverage report"
	@echo "  lint           - Run vet + golangci-lint"
	@echo "  clean          - Remove build artifacts"
	@echo "  migrate-up     - Apply DB migrations"
	@echo "  migrate-down   - Rollback last migration"
