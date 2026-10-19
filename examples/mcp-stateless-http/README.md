# Stateless streamable HTTP example

A stateless streamable HTTP MCP server (`StreamableHTTPOptions{Stateless: true}`) on protocol 2026-07-28, tracked with `rudderstackai.WrapMCPServer`. There is no session: each request carries its protocol version and client info in `_meta`. Its `delete_audience` tool asks the user to confirm by returning an `input_required` result (`InputRequests` plus an opaque `RequestState`); the client asks the user and retries the call with the answer.

```sh
# End to end against a local mock data plane, no credentials needed:
go run ./examples/mcp-stateless-http --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-stateless-http --live

# Serve it on http://127.0.0.1:8080 against your data plane:
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-stateless-http
```

`--check` serves one server to two real go-sdk clients on 2026-07-28, one that confirms and one that declines. For each, it asserts:

- one `mcp_client_seen` with `protocol_version: 2026-07-28`, `transport: streamable_http` and no `mcp_session_id`;
- one `mcp_tool_called` for `delete_audience`: the `input_required` leg sends none, the retry sends one with `call_index: 1` and no `retry_count`;
- one `mcp_elicitation_completed` with the user's `elicitation_action` (`accept` or `decline`), `tool_name`, `duration_ms` and the original request's `request_id` (not the retry's), never the question or the answer;
- every event for the anonymous system user, with `context.wrapperLibrary`, valid against the spec schema.
