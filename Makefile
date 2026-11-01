BINARY  := bazel
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local/bin
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

## build: builds the binary into ./bazel
.PHONY: build
build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

## install: builds and installs into ~/.local/bin (change with PREFIX=...)
.PHONY: install
install:
	@mkdir -p $(PREFIX)
	go build -ldflags '$(LDFLAGS)' -o $(PREFIX)/$(BINARY) .
	@echo "instalado em $(PREFIX)/$(BINARY)"

## run: builds and starts the web interface, opening the browser
.PHONY: run
run: build
	./$(BINARY) --open

## test: runs the tests
.PHONY: test
test:
	go test ./...

## e2e-setup: installs what the e2e suite needs (uses the network, once)
.PHONY: e2e-setup
e2e-setup:
	cd e2e && npm ci --no-audit --no-fund
	cd e2e && ./node_modules/.bin/playwright install chromium

## e2e: end-to-end tests with a fake gh and a fake agent (Playwright, no network)
.PHONY: e2e
e2e:
	@test -x e2e/node_modules/.bin/playwright || (cd e2e && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --offline --no-audit --no-fund) \
		|| (echo "e2e: dependencies are not in the npm cache: run make e2e-setup once" >&2; exit 1)
	cd e2e && ./node_modules/.bin/playwright test $(E2E_ARGS)

## e2e-cover: e2e suite plus the coverage of app.js it reaches
.PHONY: e2e-cover
e2e-cover:
	E2E_COVERAGE=1 $(MAKE) e2e
	node e2e/tools/coverage.mjs

## cover: runs the tests with a coverage report
.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## fmt: formats the code
.PHONY: fmt
fmt:
	gofmt -l -w .

## vet: static analysis
.PHONY: vet
vet:
	go vet ./...

## tidy: tidies go.mod
.PHONY: tidy
tidy:
	go mod tidy

## check: fmt + vet + test, what to run before committing
.PHONY: check
check: fmt vet test

## clean: removes the build artifacts
.PHONY: clean
clean:
	rm -f $(BINARY) coverage.out

## help: lists the targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  make /' | column -t -s ':'
