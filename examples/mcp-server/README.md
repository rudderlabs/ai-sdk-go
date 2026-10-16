# MCP server example

A workspace MCP server tracked with `rudderstackai.WrapMCPServer`: a list tool, a create tool with a declared `audience_created` event, the `request_context` argument with an intent list, and the `request_capability` tool.

```sh
# End to end against a local mock data plane, no credentials needed:
go run ./examples/mcp-server --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-server --live

# Serve it against your data plane:
export RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=...
go run ./examples/mcp-server --transport=stdio                     # for a client that starts it
go run ./examples/mcp-server --transport=http --addr=127.0.0.1:8080 # streamable HTTP
```

`--check` runs three legs, each a real go-sdk client exercising the server: streamable HTTP on 2025-06-18, streamable HTTP with a client on the latest protocol (a stateful server negotiates 2025-11-25 with it; see [`mcp-stateless-http`](../mcp-stateless-http) for 2026-07-28), and stdio (the example starts itself as a subprocess). Each leg must produce:

- `mcp_client_seen`: server name and version, client name and version, transport, `auth_method: none`;
- `mcp_tool_called` twice: tool name, status, the negotiated `protocol_version`, `intent` and `user_sentiment` from `request_context`, `interaction_id` and `trace_id` from `_meta`, `argument_keys`, `call_index` and `retry_count`;
- `audience_created` with `audience_type`, and `mcp_capability_requested` with `capability`;
- every event for the anonymous system user (`anonymousId: ai_system`), with `context.wrapperLibrary`, valid against the spec schema.
