# RudderStack AI SDK for Go

Standard AI product events for Go services, sent through the RudderStack Go client you already use and following the [AI events spec v0.3](spec/ai-events-v0.3.schema.json). This first release covers MCP servers: agents are the users of an MCP server, and each request they make becomes a small track event that says which AI client called, which tool, whether it worked, and for which customer.

## How it fits

```
AI client (Claude, ChatGPT, an IDE) ─ MCP ─ your go-sdk server ─ WrapMCPServer ─ analytics-go ─ RudderStack ─ warehouse, destinations
```

This is product analytics, not observability: it puts agent usage next to the rest of your customer data and complements your tracing tool. It is private by default: tool arguments, results, resource contents, prompt text and elicitation answers are never sent, and agent-written text is sent only with content capture on, masked.

The Node.js ([`@rudderstack/ai`](https://github.com/rudderlabs/ai-sdk-node)) and Python ([`rudderstack-ai`](https://github.com/rudderlabs/ai-sdk-python)) SDKs send the same events with the same properties.

## Install

```sh
go get github.com/rudderlabs/ai-sdk-go
```

Go 1.27 or later. Works with [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) v1.8 and [`github.com/rudderlabs/analytics-go/v4`](https://github.com/rudderlabs/analytics-go).

## Quick start

```go
import (
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

client, err := analytics.NewWithConfig(writeKey, analytics.Config{DataPlaneUrl: dataPlaneURL})
if err != nil {
	return err
}
defer client.Close() // flushes queued events

server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "create_audience"}, createAudience)

err = rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client,
	Identify: func(ctx context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
		if tok == nil {
			return rudderstackai.Identity{}, nil
		}
		return rudderstackai.Identity{AccountID: tok.Extra["workspace_id"].(string)}, nil
	},
	EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}},
	RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
})
if err != nil {
	return err
}
// Serve as usual: server.Run(ctx, &mcp.StdioTransport{}) or mcp.NewStreamableHTTPHandler(...).
```

Call `WrapMCPServer` once, before serving. It adds a receiving and a sending middleware to the server; requests and responses pass through unchanged except for the optional `request_context` argument (added to each tool's input schema and removed before your handler runs).

## Events

| Event                                                       | When                                                                     |
| ----------------------------------------------------------- | ------------------------------------------------------------------------ |
| `mcp_client_seen`                                           | The first request of a (client, OAuth client, user) in a 24-hour window  |
| `mcp_tool_called`                                           | A `tools/call` completes, fails or is cancelled                          |
| `mcp_elicitation_completed`                                 | The user answered (or did not answer) a question the server asked        |
| `mcp_resource_read`, `mcp_prompt_used`                      | A `resources/read` or `prompts/get` completes or fails                   |
| Declared tool events, e.g. `audience_created`               | A call of a tool with a rule in `EventRules`                             |
| `mcp_capability_requested`                                  | Opt-in: the agent called the injected `request_capability` tool          |
| `mcp_event_subscription_changed`, `mcp_event_delivered`     | Experimental, opt-in: the MCP Events draft extension                     |

Every event carries `context.wrapperLibrary` (`name: "ai-sdk-go"`, `version`, `specVersion: "0.3.0"`, `integration: "mcp"`) beside analytics-go's own `context.library`. Both MCP protocol revisions are supported: 2025-06-18 (initialize, sessions) and 2026-07-28 (stateless, client details in each request's `_meta`).

See [docs/mcp.md](docs/mcp.md) for every option, how each property is derived, identity, joining the calling agent through `_meta`, and limits.

## Examples

| Example                                               | Shows                                                                                         |
| ----------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| [`mcp-server`](examples/mcp-server)                   | stdio and streamable HTTP, `request_context`, a declared event, `request_capability`          |
| [`mcp-stateless-http`](examples/mcp-stateless-http)   | Stateless streamable HTTP on 2026-07-28, confirmation through an `input_required` elicitation |
| [`mcp-auth`](examples/mcp-auth)                       | A bearer token check and `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |

`go run ./examples/<name> --check` runs one end to end against a local mock data plane and fails on a missing or wrong event; `--live` sends the same events to `RUDDERSTACK_DATA_PLANE_URL` with `RUDDERSTACK_WRITE_KEY`. See [examples/README.md](examples/README.md).

## Reliability

Analytics never breaks the server: the integration recovers from its own panics, a failing `Identify` or rule function only drops what it would have added, and a client error drops the event. State is in memory, per wrapped server, per process: each replica and each restart emits its own `mcp_client_seen` and restarts the `call_index`, `retry_count` and `ms_since_previous_call` counters.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE.md)
