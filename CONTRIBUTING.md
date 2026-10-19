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
*.go                  package rudderstackai: the public API, at the module root
version.go            SDK version, bumped by release-please
spec/                 AI events spec JSON Schema and cross-SDK fixtures shared with the Node.js and Python SDKs
internal/dataplane/   mock RudderStack data plane, optionally forwarding to a real one
examples/             MCP examples; each runs end to end in CI with --check, and nightly with --live
```

## Everyday commands

| Command          | What it does                                                                     |
| ---------------- | -------------------------------------------------------------------------------- |
| `make build`     | `go build ./...`                                                                 |
| `make lint`      | golangci-lint (includes gofmt, goimports, govet, staticcheck, revive, misspell)  |
| `make test`      | `go test -race` with a coverage profile in `coverage.out`                        |
| `make cover`     | Runs the tests, then prints per-function and total coverage                      |
| `make vulncheck` | govulncheck against the module and its dependencies                              |
| `make example`   | Runs every example with `--check`                                                |

CI also runs `gofmt -l .` and `go vet ./...` on their own, and fails if total coverage drops below 90%.

## Testing rules

- Tests use the real libraries (`github.com/modelcontextprotocol/go-sdk`, `github.com/rudderlabs/analytics-go/v4`). Only the network is replaced, with local `httptest` servers or in-memory transports. Don't mock what you can use for real.
- Every event emitted in a test is validated against the spec JSON Schema in `spec/`.
- Keep behaviour and payloads identical to the Node.js (`rudderlabs/ai-sdk-node`) and Python (`rudderlabs/ai-sdk-python`) SDKs: event and property names, integration tags, `context.wrapperLibrary`, error types and option semantics. Differences are allowed only for true platform limits.
- Cross-SDK fixtures in `spec/fixtures/` are byte copies of the Node.js SDK's; update them there first.

## Dependencies

Everything is pinned exactly: `go.mod` versions, tool versions in the `Makefile` and CI, and GitHub Actions (by full commit SHA with a version comment). Dependabot proposes updates weekly after a 7-day cooldown.

## Tooling choices

- **golangci-lint v2** with the standard linter set plus revive and misspell, and gofmt/goimports as formatters (`.golangci.yml`).
- **govulncheck** reports only vulnerabilities in code paths the module actually calls.
- **release-please** (manifest mode, `go` release type) for versioning and changelogs, as in `ai-sdk-node` and `ai-sdk-python`.

## Commits and pull requests

- [Conventional Commits](https://www.conventionalcommits.org/) are required (checked by the PR title check in CI); release-please builds the changelog from them. Use a scope for integration-specific changes, e.g. `fix(mcp): ...`.
- Open pull requests against `main`; they are squash-merged, so the PR title becomes the commit message.
- Add or update tests with every change, and update the README when the public API changes. Exported identifiers need Go doc comments.
