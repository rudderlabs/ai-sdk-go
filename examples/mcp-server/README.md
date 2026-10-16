# MCP server example

A workspace MCP server tracked with `rudderstackai.WrapMCPServer`: a paginated list tool, a create tool with a declared `audience_created` event, the `request_context` argument with an intent list, and the `request_capability` tool.

```sh
# End to end against a local mock data plane, no credentials needed:
go run ./examples/mcp-server --check

# Against your RudderStack data plane:
export RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=...
go run ./examples/mcp-server --transport=stdio                     # for a client that starts it
go run ./examples/mcp-server --transport=http --addr=127.0.0.1:8080 # streamable HTTP
```

`--check` runs three legs, each a real go-sdk client exercising the server: streamable HTTP on 2025-06-18, streamable HTTP on 2026-07-28, and stdio (the example starts itself as a subprocess). Each leg must produce `mcp_client_seen`, two `mcp_tool_called`, `audience_created` and `mcp_capability_requested` with the expected `context.wrapperLibrary`, transport, intent, sentiment and joined `interaction_id` / `trace_id`. The tests also validate every event against the spec schema.
