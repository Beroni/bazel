BINARY  := bazel
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local/bin
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

## build: compila o binário em ./bazel
.PHONY: build
build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

## install: compila e instala em ~/.local/bin (mude com PREFIX=...)
.PHONY: install
install:
	@mkdir -p $(PREFIX)
	go build -ldflags '$(LDFLAGS)' -o $(PREFIX)/$(BINARY) .
	@echo "instalado em $(PREFIX)/$(BINARY)"

## run: compila e sobe a interface web, abrindo o navegador
.PHONY: run
run: build
	./$(BINARY) --open

## test: roda os testes
.PHONY: test
test:
	go test ./...

## cover: roda os testes com relatório de cobertura
.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

## fmt: formata o código
.PHONY: fmt
fmt:
	gofmt -l -w .

## vet: análise estática
.PHONY: vet
vet:
	go vet ./...

## tidy: arruma o go.mod
.PHONY: tidy
tidy:
	go mod tidy

## check: fmt + vet + test, o que rodar antes de commitar
.PHONY: check
check: fmt vet test

## clean: remove os artefatos de build
.PHONY: clean
clean:
	rm -f $(BINARY) coverage.out

## help: lista os alvos
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  make /' | column -t -s ':'
