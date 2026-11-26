# Examples

Each example is a `main` package using `rudderstackai` as an app would.

- [`mcp-server`](mcp-server): Workspace tools, request context and feedback over stdio and streamable HTTP.
- [`mcp-server-mcpgo`](mcp-server-mcpgo): The workspace server tracked with `ruddermcpgo.WrapServer`, including HTTP on 2026-07-28.
- [`mcp-stateless-http`](mcp-stateless-http): Confirmation through an `input_required` result and a retry on 2026-07-28.
- [`mcp-auth`](mcp-auth): Bearer token verification and customer identity over streamable HTTP.
- [`openai`](openai): A streamed Chat Completions answer, pricing and `TrackAction` feedback.
- [`anthropic`](anthropic): A streamed Messages answer with cache read and write pricing.
- [`google-genai`](google-genai): A Gemini tool call followed by a streamed answer with `tool_breakdown`.
- [`bedrock-converse`](bedrock-converse): `Converse` and `ConverseStream` with inference profile pricing.
- [`background-agent`](background-agent): A scheduled `RunTask`, a chained sub-task and `TrackScore`.
- [`approval-loop`](approval-loop): `RunTask` pauses twice for approval and resumes from a stored `PausedInteraction` in a later worker. It emits `ai_task_started`.
- [`manual`](manual): Manual recording, a declared tool event, per-call events and a custom classifier.
- [`tracing`](tracing): OpenTelemetry span input, product events and trace joins recorded locally.

## Setup

Install Go 1.26.11 or later, as required by `examples/go.mod`. Run from a clone of the repository so checks can read the shared schema and cassettes in `spec/`. From the repository root, run `go mod download`, as in [contributing](../CONTRIBUTING.md#setup).

Live runs need `RUDDERSTACK_WRITE_KEY` for your RudderStack source and `RUDDERSTACK_DATA_PLANE_URL` for your data plane. Run `export RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=...` before serving an MCP example. Without flags, provider and manual examples run once. Provider credentials are listed in each README. Without the RudderStack variables, events from these runs go to a local data plane and are printed. MCP examples serve a real client. Each README gives its exact commands and extra credentials.

```sh
go run ./examples/mcp-auth --check
go run ./examples/mcp-auth --live
go run ./examples/anthropic
```

Content capture is off by default. See [privacy](../docs/privacy.md) before enabling it.

## Tests

Run `make example` to check every example. Run `go test ./examples/...` for their package tests. CI checks examples on each pull request. `--check` needs no credentials. It uses a local mock data plane from `internal/dataplane`, real go-sdk MCP or provider clients, and the real analytics-go client. Provider checks replay shared cassettes in `spec/fixtures/cassettes/` through `internal/testkit`'s mock LLM server. Checks fail unless expected events arrive with the expected properties, field by field. Every event is checked for `context.wrapperLibrary` and validated against `spec/ai-events-v0.4.schema.json`.

`--live` runs the same check. The local data plane forwards each batch with the write key to `RUDDERSTACK_DATA_PLANE_URL` and keeps only accepted batches. Without both variables, it prints `skipped: no key` and exits 0. The `Live examples` workflow runs nightly and on demand. Missing secrets are reported as a skip in the job summary.

The asserts hold with `mcp_client_seen` windows and call counters per wrapped server or per server name. Each agent in a check uses its own client name and interaction ID.
