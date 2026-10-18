# Every build carries TAGS: without ts_omit_peerapiserver, tsnet's peer API
# listens on every host interface and storewarden refuses to start
# (docs/adr/0003-build-without-the-peer-api-server.md).
TAGS := ts_omit_peerapiserver
BIN  ?= bin

# Analysis tools run at pinned versions with `go run tool@version`, so they
# stay out of go.mod. CI uses the same versions through these targets.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

.PHONY: check ci fmt vet never-public test lint vuln build smoke hooks

# check is what the pre-commit hook runs. It needs no network.
check: fmt vet never-public test

# ci is check plus the tools that download themselves or the vulnerability
# database. The GitHub Actions workflow runs each of these as its own step.
ci: check lint vuln

fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

# Vet both build variants, so code behind either one still compiles.
vet:
	go vet ./...
	go vet -tags $(TAGS) ./...

# Fails if non-test code could make storewarden reachable from outside the
# tailnet: Funnel, serve config, a host listener, or a tsnet program that
# doesn't refuse to run with the peer API server built in.
never-public:
	go run ./scripts/never-public

test:
	go test -race -tags $(TAGS) ./...

lint:
	go run $(STATICCHECK) ./...
	go run $(STATICCHECK) -tags $(TAGS) ./...

vuln:
	go run $(GOVULNCHECK) -tags $(TAGS) ./...

build:
	go build -tags $(TAGS) -o $(BIN)/ ./cmd/...

smoke:
	./scripts/smoke-tailnet.sh

hooks:
	git config core.hooksPath .githooks
