GO ?= go
# Must match GOLANGCI_LINT_VERSION and GOVULNCHECK_VERSION in .github/workflows/ci.yml.
GOLANGCI = github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
govulncheck = golang.org/x/vuln/cmd/govulncheck@v1.8.0

.PHONY: build lint test cover vulncheck example

build:
	$(GO) build ./...

lint:
	$(GO) run $(GOLANGCI) run

test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...

cover: test
	$(GO) tool cover -func=coverage.out

vulncheck:
	$(GO) run $(govulncheck) ./...

EXAMPLES = mcp-server mcp-stateless-http mcp-auth

example:
	for e in $(EXAMPLES); do $(GO) run ./examples/$$e --check || exit 1; done
