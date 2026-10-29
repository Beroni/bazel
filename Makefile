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
