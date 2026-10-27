# Contributing to github.com/rudderlabs/ai-sdk-go

Thanks for helping improve the SDK. By contributing you agree to the [RudderStack Contributor Agreement](https://forms.gle/845JRGVZaC6kPZy68).

## Setup

Install the Go version in `go.mod` (or let `GOTOOLCHAIN=auto` fetch it), then:

```sh
go mod download
```

Tools (golangci-lint, govulncheck) run through `go run` at the versions pinned in the `Makefile`; nothing else to install.

## Layout

```
*.go                    package rudderstackai: the public API (interactions, tasks, Middleware, MCP), at the module root
version.go              SDK version, bumped by release-please
ruddermcpgo/            mcp-go support, in the root module
ruddertest/             Recorder: an analytics-go client that records to JSONL for the migration check, in the root module
cmd/aicheck/            module of the aicheck migration check command (go install ...@latest), with its own copy of the schemas
rudderopenai/ rudderanthropic/ ruddergenai/ rudderbedrock/ rudderotel/
                        one Go module per provider integration, plus the OpenTelemetry trace_id helper
internal/kit/           shared HTTP interception, stream and retry handling of the integrations
internal/hook/          the link between the integrations and the interaction internals
internal/testkit/       mock LLM server, cassettes, schema validation for tests
internal/dataplane/     mock RudderStack data plane, optionally forwarding to a real one
internal/conformance/   module running the shared spec fixtures against every integration
spec/                   AI events spec JSON Schema and cross-SDK fixtures shared with the Node.js and Python SDKs
examples/               module of runnable examples; each runs end to end in CI with --check, and nightly with --live
```

`go.work` ties the modules together for development; every module also builds alone (`GOWORK=off`), as users build it.

## Everyday commands

Each target covers every module of `go.work`.

| Command          | What it does                                                                     |
| ---------------- | -------------------------------------------------------------------------------- |
| `make build`     | `go build` of every package                                                      |
| `make lint`      | golangci-lint (includes gofmt, goimports, govet, staticcheck, revive, misspell)  |
| `make test`      | `go test -race` with a coverage profile in `coverage.out`                        |
| `make cover`     | Runs the tests, then prints per-function and total coverage                      |
| `make vulncheck` | govulncheck on each module alone, against its own `go.mod`                       |
| `make tidy`      | `go mod tidy` on each module alone                                               |
| `make example`   | Runs every example with `--check`                                                |

CI also runs `gofmt -l .` and `go vet` on their own, tests on the minimum and latest supported Go, builds, tests and vulnchecks each module alone at the minimum versions in its `go.mod`, tests `ruddermcpgo` against the latest mcp-go, and fails if total coverage drops below 90%.

## Testing rules

- Tests use the real libraries (the provider SDKs, the MCP libraries, `github.com/rudderlabs/analytics-go/v4`). Only the network is replaced, with local `httptest` servers (`internal/testkit`'s mock LLM, `internal/dataplane`) or in-memory transports. Don't mock what you can use for real.
- Every event emitted in a test is validated against the spec JSON Schema in `spec/`.
- Keep behaviour and payloads identical to the Node.js (`rudderlabs/ai-sdk-node`) and Python (`rudderlabs/ai-sdk-python`) SDKs: event and property names, integration tags, `context.wrapperLibrary`, error types and option semantics. Differences are allowed only for true platform limits.
- Cross-SDK fixtures in `spec/fixtures/` are byte copies of the Node.js SDK's; update them there first, then copy them here. `internal/conformance` runs every scenario that applies to Go and names why it skips the others (no Go version of the framework).

## Dependencies

Everything is pinned exactly: `go.mod` versions, tool versions in the `Makefile` and CI, and GitHub Actions (by full commit SHA with a version comment). Dependabot proposes updates weekly after a 7-day cooldown.

The library modules require the oldest MCP and provider SDK versions they are tested against, so adding one never upgrades a consumer's SDK. Raise those minimums deliberately; Dependabot ignores them there and keeps updating the examples, conformance runner and CI tools.

## Tooling choices

- **golangci-lint v2** with the standard linter set plus revive and misspell, and gofmt/goimports as formatters (`.golangci.yml`).
- **govulncheck** reports only vulnerabilities in code paths the module actually calls.
- **release-please** (manifest mode, `go` release type) for versioning and changelogs, as in `ai-sdk-node` and `ai-sdk-python`.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/) are required (checked by the PR title check in CI); release-please builds the changelog from them. Use a scope for integration-specific changes, e.g. `fix(mcp): ...`.
- Open pull requests against `main`; they are squash-merged, so the PR title becomes the commit message.
- Add or update tests with every change, and update the README and `docs/` when the public API or behaviour changes. Exported identifiers need Go doc comments.
