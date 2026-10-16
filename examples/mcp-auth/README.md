# Authenticated MCP server example

A streamable HTTP MCP server behind `auth.RequireBearerToken`, tracked with `rudderstackai.WrapMCPServer` and an `Identify` func. The verifier stands in for your OAuth token verification; `Identify` maps the verified token's claims to `account_id` (the workspace) and `oauth_client_id`. `userId` and `token_scopes` default to the token's subject and scopes. The token itself is never sent.

```sh
# End to end against a local mock data plane, no credentials needed:
go run ./examples/mcp-auth --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-auth --live

# Serve it on http://127.0.0.1:8080; clients send Authorization: Bearer <MCP_DEMO_TOKEN>:
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... MCP_DEMO_TOKEN=... go run ./examples/mcp-auth
```

`--check` serves one server to two users of two workspaces, each through a different OAuth client, then sends a request without a token. For each user, it asserts:

- one `mcp_client_seen` with `userId` set to the token subject, `auth_method: oauth`, `oauth_client_id`, `token_scopes`, `account_id`, the session ID and the transport;
- two `mcp_tool_called` with the same `userId`, `account_id` and session, `call_index` 0 and 1, and `retry_count` 0 and 1 (the same call twice);
- no events for the request without a token (401), no bearer token in any event, and every event valid against the spec schema.
