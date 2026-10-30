GO ?= go
# Must match GOLANGCI_LINT_VERSION and GOVULNCHECK_VERSION in .github/workflows/ci.yml.
GOLANGCI = github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
govulncheck = golang.org/x/vuln/cmd/govulncheck@v1.9.0

# Every module of go.work; ./... alone covers only the root module.
MODULES = . rudderopenai rudderanthropic ruddergenai rudderbedrock rudderotel cmd/aicheck internal/conformance examples
PKGS = ./... $(addsuffix /...,$(addprefix ./,$(filter-out .,$(MODULES))))

.PHONY: build lint test cover vulncheck tidy example

build:
	$(GO) build $(PKGS)

lint:
	$(GO) run $(GOLANGCI) run $(PKGS)

test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKGS)

cover: test
	$(GO) tool cover -func=coverage.out

# Each module on its own, as users build it: its own go.mod, no workspace.
vulncheck:
	for m in $(MODULES); do (cd $$m && GOWORK=off $(GO) run $(govulncheck) ./...) || exit 1; done

tidy:
	for m in $(MODULES); do (cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; done

EXAMPLES = mcp-server mcp-server-mcpgo mcp-stateless-http mcp-auth openai anthropic google-genai bedrock-converse background-agent approval-loop manual

example:
	for e in $(EXAMPLES); do $(GO) run ./examples/$$e --check || exit 1; done
