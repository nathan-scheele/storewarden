# Every build carries TAGS: without ts_omit_peerapiserver, tsnet's peer API
# listens on every host interface and storewarden refuses to start
# (docs/adr/0003-build-without-the-peer-api-server.md).
TAGS := ts_omit_peerapiserver
BIN  ?= bin

.PHONY: check fmt vet test lint build smoke hooks

# check is what the pre-commit hook and CI run.
check: fmt vet test

fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

# Vet both build variants, so code behind either one still compiles.
vet:
	go vet ./...
	go vet -tags $(TAGS) ./...

test:
	go test -race -tags $(TAGS) ./...

lint:
	staticcheck -tags $(TAGS) ./...

build:
	go build -tags $(TAGS) -o $(BIN)/ ./cmd/...

smoke:
	./scripts/smoke-tailnet.sh

hooks:
	git config core.hooksPath .githooks
