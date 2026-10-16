# Examples

Each example is a `main` package in this module that uses `rudderstackai` the way an app would.

| Example                                    | Shows                                                                                                          |
| ------------------------------------------ | -------------------------------------------------------------------------------------------------------------- |
| [`mcp-server`](mcp-server)                 | `WrapMCPServer` over stdio and streamable HTTP: `request_context`, a declared `audience_created` event, `request_capability` |
| [`mcp-stateless-http`](mcp-stateless-http) | Stateless streamable HTTP on 2026-07-28: a confirmation asked through an `input_required` result and answered in the retry |
| [`mcp-auth`](mcp-auth)                     | Streamable HTTP behind a bearer token check, with `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |

None needs a provider key. Live runs need `RUDDERSTACK_WRITE_KEY` and `RUDDERSTACK_DATA_PLANE_URL` (a RudderStack source's write key and your data plane URL).

## Run

From the repository root:

```sh
go run ./examples/mcp-auth --check   # end to end against a local mock data plane, no credentials
go run ./examples/mcp-auth --live    # the same, with the events sent to your data plane
go run ./examples/mcp-auth           # serve it, for a real MCP client
```

## How the checks work

`--check` starts a mock data plane (`internal/dataplane`), drives the server with a real go-sdk MCP client and the real analytics-go client, and fails unless every expected event arrives with the expected properties, field by field. Every event is also checked for `context.wrapperLibrary` and validated against `spec/ai-events-v0.3.schema.json`. CI runs every example this way on each pull request (`make example`; `go test ./...` runs them too).

`--live` runs the same check, but the local data plane forwards each batch, write key included, to `RUDDERSTACK_DATA_PLANE_URL` and keeps only the batches it accepts, so the asserts cover what your data plane received. Without both variables it prints `skipped: no key` and exits 0. The `Live examples` workflow runs every example this way nightly and on demand, and writes the skip to the job summary when the repository has no secrets.

The asserts hold whether the SDK keeps `mcp_client_seen` windows and call counters per wrapped server or per server name: each agent in a check uses its own client name and interaction ID.
