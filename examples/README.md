# Examples

Each example is a `main` package in this module that uses `rudderstackai` the way an app would.

| Example                                    | Shows                                                                                                          |
| ------------------------------------------ | -------------------------------------------------------------------------------------------------------------- |
| [`mcp-server`](mcp-server)                 | `WrapMCPServer` over stdio and streamable HTTP: `request_context`, a declared `audience_created` event, `request_capability` |
| [`mcp-server-mcpgo`](mcp-server-mcpgo)     | The same server on `github.com/mark3labs/mcp-go`, tracked with `ruddermcpgo.WrapServer`, including 2026-07-28 over streamable HTTP |
| [`mcp-stateless-http`](mcp-stateless-http) | Stateless streamable HTTP on 2026-07-28: a confirmation asked through an `input_required` result and answered in the retry |
| [`mcp-auth`](mcp-auth)                     | Streamable HTTP behind a bearer token check, with `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |
| [`openai`](openai)                         | `rudderopenai`: a streamed Chat Completions answer in `RunInteraction`, a registered price, `TrackAction` feedback |
| [`anthropic`](anthropic)                   | `rudderanthropic`: a streamed Messages answer with a cache read and write, priced with cache rates |
| [`google-genai`](google-genai)             | `ruddergenai`: `generateContent` calling a tool, then the streamed answer, with `tool_breakdown` |
| [`bedrock-converse`](bedrock-converse)     | `rudderbedrock`: `Converse` calling a tool, then `ConverseStream`, priced through an inference profile |
| [`background-agent`](background-agent)     | `RunTask` on a schedule (`account_id`, `on_behalf_of`), a chained sub-task (`parent_interaction_id`), `TrackScore` |
| [`manual`](manual)                         | No integration: `StartInteraction`, `RecordCall`, `RecordToolCalls`, `RecordToolResult`, `End`, a declared tool event, per-call events, a custom classifier |

The checks need no provider key. Without flags, a provider example runs once against the real provider: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` (or `GOOGLE_API_KEY`), or `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION`. Live runs need `RUDDERSTACK_WRITE_KEY` and `RUDDERSTACK_DATA_PLANE_URL` (a RudderStack source's write key and your data plane URL); a provider run without them sends its events to a local data plane and prints them.

## Run

From the repository root:

```sh
go run ./examples/mcp-auth --check   # end to end against a local mock data plane, no credentials
go run ./examples/mcp-auth --live    # the same, with the events sent to your data plane
go run ./examples/mcp-auth           # serve it, for a real MCP client
go run ./examples/anthropic          # one real Anthropic call (ANTHROPIC_API_KEY), events printed
```

## How the checks work

`--check` starts a mock data plane (`internal/dataplane`), drives the example with real clients (a go-sdk MCP client, or the provider's own SDK pointed at `internal/testkit`'s mock LLM server, which replays the shared cassette in `spec/fixtures/cassettes/`) and the real analytics-go client, and fails unless every expected event arrives with the expected properties, field by field. Every event is also checked for `context.wrapperLibrary` and validated against `spec/ai-events-v0.3.schema.json`. CI runs every example this way on each pull request (`make example`; `go test ./...` runs them too).

`--live` runs the same check, but the local data plane forwards each batch, write key included, to `RUDDERSTACK_DATA_PLANE_URL` and keeps only the batches it accepts, so the asserts cover what your data plane received. Without both variables it prints `skipped: no key` and exits 0. The `Live examples` workflow runs every example this way nightly and on demand, and writes the skip to the job summary when the repository has no secrets.

The asserts hold whether the SDK keeps `mcp_client_seen` windows and call counters per wrapped server or per server name: each agent in a check uses its own client name and interaction ID.
