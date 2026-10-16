# MCP servers

`rudderstackai.WrapMCPServer` tracks an MCP server built on [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk): which AI clients call it, which tools they call, whether calls succeed, and for which customer. The agents are the users. Events are sent from the server; they never carry tool arguments, results, resource contents, prompt text or elicitation answers.

- [Setup](#setup)
- [Options](#options)
- [Events](#events)
- [Protocol revisions](#protocol-revisions)
- [Identity](#identity)
- [Joining the calling agent](#joining-the-calling-agent)
- [Agent-reported context](#agent-reported-context)
- [Declared tool events](#declared-tool-events)
- [Unmet demand: request_capability](#unmet-demand-request_capability)
- [Elicitation](#elicitation)
- [MCP Events (experimental)](#mcp-events-experimental)
- [Limits](#limits)

## Setup

```go
server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
// server.AddTool / mcp.AddTool ...

if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client, // analytics.Client from github.com/rudderlabs/analytics-go/v4
	Identify:  identify,
	EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}},
	RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
	Transport:      rudderstackai.TransportStdio, // for a stdio server; streamable HTTP is detected
}); err != nil {
	log.Fatal(err)
}
```

Call it once, before serving: it adds a receiving middleware (`AddReceivingMiddleware`) and a sending middleware (`AddSendingMiddleware`), so it works with every transport the go-sdk offers. Wrapping the same server again replaces its options and keeps its state. `WrapMCPServer` returns an error for invalid options. Close the analytics client when the process stops (`defer client.Close()`), which flushes queued events.

## Options

| Field                | Default    | What it does                                                                                                                                                                      |
| -------------------- | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Analytics`          | required   | The RudderStack client (`analytics.Client`).                                                                                                                                      |
| `Identify`           | none       | `func(ctx, *auth.TokenInfo) (Identity, error)`, called once per request with the verified bearer token (`nil` without one). Returns `UserID`, `AnonymousID`, `AccountID`, `OAuthClientID`, `TokenScopes`, `AuthMethod`, `Country`. |
| `EventRules`         | none       | Declared tool events. Names starting with `ai_` or `mcp_` are rejected.                                                                                                          |
| `RequestContext`     | on         | `Intents` for the injected `request_context` argument; `DisableUserSentiment`; `Disabled` turns it off.                                                                          |
| `ClientSeenWindow`   | 24 hours   | Window in which a (client, OAuth client, user) is reported once by `mcp_client_seen`, per wrapped server and process.                                                                                            |
| `CaptureContent`     | off        | Sends `call_reason` and `capability_detail`, masked and capped at 200 characters.                                                                                                |
| `CapabilityRequests` | off (nil)  | `&CapabilityRequestOptions{Capabilities: []string{"bulk_export"}}` adds the `request_capability` tool.                                                                         |
| `MCPEvents`          | off        | Enables the experimental [MCP Events](#mcp-events-experimental) helpers.                                                                                                          |
| `Transport`          | detected   | `TransportStdio` or `TransportSSE`, sent as `transport` on `mcp_client_seen`. Streamable HTTP is detected from the request headers; stdio and SSE look the same from inside the server. |

## Events

Every event is a track call with `context.wrapperLibrary` = `{name: "ai-sdk-go", version, specVersion: "0.3.0", integration: "mcp"}` and, when known, `server_name`, `account_id`, `mcp_session_id`, `client_name`, `client_version`, `protocol_version`, `interaction_id` and `trace_id`. Schemas: [`spec/ai-events-v0.3.schema.json`](../spec/ai-events-v0.3.schema.json). Events without a user are sent with `anonymousId: "ai_system"`.

| Event                            | When                                                                   | Main properties |
| -------------------------------- | ---------------------------------------------------------------------- | --------------- |
| `mcp_client_seen`                | The first request of a (client name, OAuth client, user) in the window | `server_version`, `client_title`, `client_capabilities`, `transport`, `auth_method`, `oauth_client_id`, `token_scopes`, `country` |
| `mcp_tool_called`                | A `tools/call` completes, fails or is cancelled                        | `request_id`, `tool_name`, `tool_status`, `error_type`, `is_error_result`, `duration_ms`, `time_to_cancel_ms`, `argument_keys`, `invalid_arguments`, `retry_count`, `call_index`, `ms_since_previous_call`, `progress_requested`, `result_size_bytes`, `result_tokens_estimate`, `result_content_types`, `result_paginated`, `intent`, `user_sentiment`, `call_reason` |
| `mcp_elicitation_completed`      | The client answered (or failed to answer) a question the server asked  | `elicitation_action`, `tool_name`, `request_id`, `duration_ms`, `error_type` |
| `mcp_resource_read`              | A `resources/read` completes or fails                                  | `request_id`, `resource_name` (the registered name or URI template, never the URI), `tool_status`, `error_type`, `duration_ms`, `result_size_bytes` |
| `mcp_prompt_used`                | A `prompts/get` completes or fails                                     | `request_id`, `prompt_name`, `tool_status`, `error_type`, `duration_ms`, `result_size_bytes` |
| Declared tool events             | A call of a tool with a rule                                           | `tool_name`, `tool_status` and the rule's properties |
| `mcp_capability_requested`       | The agent called `request_capability` (opt-in)                         | `capability`, `capability_detail` |
| `mcp_event_subscription_changed` | You record an MCP Events subscription change (experimental)            | `subscription_action`, `subscription_id`, `event_name`, `argument_keys`, `delivery_mode`, `ttl_ms`, `failure_reason` |
| `mcp_event_delivered`            | A wrapped sender finishes one delivery (experimental)                  | `subscription_id`, `event_name`, `event_id`, `delivery_status`, `http_status`, `attempt_count`, `delivery_latency_ms`, `payload_size_bytes` |

How the `mcp_tool_called` signals are derived:

- **`tool_status` and `error_type`**: a result with `IsError` is `error` with `is_error_result: true` (`mcp.AddTool` turns a handler error and an argument validation failure into one); its `error_type` is `invalid_request` when arguments failed the schema, else `api_error`. A returned error maps by JSON-RPC code: parse error, invalid request, method not found and invalid params are `invalid_request`, request timeout (-32001) `timeout`, connection closed (-32000) `connection_error`, others `api_error`; a context deadline is `timeout`. A call the client cancels (`notifications/cancelled`) is `cancelled` with `time_to_cancel_ms` instead of `duration_ms`.
- **`invalid_arguments`**: on a failed call, the names of the arguments that fail the tool's input schema one level deep (missing required, wrong `type`, not in `enum`, not `const`, unexpected when `additionalProperties: false`). Needs the schema from a `tools/list` this process answered.
- **`call_index`, `ms_since_previous_call` and `retry_count`**: counted per wrapped server and process, per `mcp_session_id`, else per caller-sent `interaction_id`. `call_index` starts at 0; `ms_since_previous_call` is the gap since the previous call in the scope ended; `retry_count` counts earlier calls of the same tool with the same arguments (a SHA-256 hash of their canonical JSON, kept in memory). Without either scope (a minted `interaction_id`) the three are left out.
- **`argument_keys`**: the argument names, sorted, without `request_context`.
- **Result**: `result_size_bytes` is the UTF-8 size of the result's compact JSON (no HTML escaping), `result_tokens_estimate` that size divided by 4 (rounded down), `result_content_types` the content block types plus `structured` for `structuredContent`, `result_paginated` whether the result or its `structuredContent` carries a `nextCursor` (or `next_cursor`).

## Protocol revisions

The go-sdk v1.8 serves both revisions, and both are tested end to end with a real go-sdk client.

|                                | 2025-06-18                                                     | 2026-07-28 (stateless)                                                                                       |
| ------------------------------ | -------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| Client name, version, features | The `initialize` request                                       | Each request's `_meta`: `io.modelcontextprotocol/clientInfo`, `io.modelcontextprotocol/clientCapabilities`   |
| `protocol_version`             | Negotiated in `initialize` (else the `Mcp-Protocol-Version` header) | `io.modelcontextprotocol/protocolVersion` of each request                                              |
| `mcp_client_seen`              | At `initialize`                                                | At the first request in the window (`server/discover` included)                                              |
| `mcp_session_id`               | The transport's session ID (`Mcp-Session-Id` on streamable HTTP) | Absent                                                                                                     |
| Elicitation                    | The server's `elicitation/create` request and the client's response | A `tools/call` result with `inputRequests`, answered by the client's retry with `inputResponses` (matched by `requestState`, else by client, user and tool) |

The `input_required` leg of a call sends no `mcp_tool_called`; the retry that completes it does, without `retry_count`. The elicitation's `request_id` is the original request's and `duration_ms` runs from the `input_required` result to the retry. A handler that returns `InputRequests` to a 2025-06-18 client is answered by the go-sdk through `elicitation/create`, which is recorded the same way as a direct `session.Elicit`.

## Identity

`Identify(ctx, token)` receives the `*auth.TokenInfo` that `auth.RequireBearerToken` verified (nil over stdio or without auth) and returns the user and workspace. `UserID` should be the OAuth subject and `AccountID` the workspace. Defaults: `UserID` from `token.UserID`, `TokenScopes` from `token.Scopes`, `AuthMethod` `oauth` with a token, else `none`. `OAuthClientID` has no default: `auth.TokenInfo` does not carry the client ID, so return it from your verifier's `Extra`. The token itself and the client's IP address are never sent; return a coarse `Country` if you derive one. If `Identify` fails or panics, only the token's own user ID and scopes are used.

## Joining the calling agent

The server reads exactly these keys from a request's `_meta`:

- `com.rudderstack/interaction_id` becomes `interaction_id`;
- the W3C `traceparent` trace-id becomes `trace_id`, else `com.rudderstack/trace_id`.

Values are opaque strings capped at 256 characters; no other `_meta` content is copied. Without them the server mints a random `interaction_id` per request and sends no `trace_id`. An agent tracked with the Node.js or Python SDK passes its interaction's ID, so each `mcp_tool_called` joins the `ai_response_received` that made the call.

## Agent-reported context

The server never sees the user's prompt. To learn why a tool was called, the integration adds one optional `request_context` object to each tool's input schema in `tools/list`, removes it from the arguments before the handler runs, and records it on `mcp_tool_called`:

- `intent`: from `RequestContext.Intents` (plus `other`); a value not in the list is sent as `other`. Without a list, `intent` is not offered. Avoid sensitive categories: the label alone can reveal them.
- `user_sentiment`: `negative`, `neutral` or `positive`. `DisableUserSentiment` drops it.
- `call_reason`: one sentence on why the agent made the call; offered and sent only with `CaptureContent`, masked and capped at 200 characters.

A tool that already has a `request_context` argument keeps its own and receives it unchanged. The schema is added to copies of the tool definitions, never the server's own. Cost: a few tokens in every tool definition and a few output tokens per call, paid by whoever runs the agent.

## Declared tool events

`EventRules.Tools` maps a tool name to a `ToolEventRule{Event, Properties, When}`. `Properties(args, result)` and `When(args, result)` see the call's arguments without `request_context` and the result's `structuredContent`, else its single text block parsed as JSON. The event carries `tool_name`, `tool_status`, the common MCP fields and the rule's properties (it cannot override the integration's own). If `Properties` panics the event is sent without them; if `When` panics the event is not sent.

## Unmet demand: request_capability

With `CapabilityRequests: &rudderstackai.CapabilityRequestOptions{Capabilities: []string{"bulk_export", "edit_permissions"}}`, `tools/list` gains a `request_capability` tool (arguments `capability` and, with `CaptureContent`, `detail`) the agent calls when no tool can do what the user needs. The integration answers it itself (your server never sees the call) and sends `mcp_capability_requested` with `capability` (a listed gap, else `other`). With `CaptureContent`, an `other` request also carries `capability_detail`: masked, capped at 200 characters, and withheld if it still matches a sensitive pattern. A server that defines its own `request_capability` tool keeps it, and nothing is injected.

## Elicitation

Elicitations that reach the client are recorded by the sending middleware: the action the client returned, or `cancel` with `error_type` (`timeout` when the server's context deadline passed, `cancelled` when it was cancelled, else by JSON-RPC code). `session.Elicit` rejects a client without elicitation support before anything is sent, which no middleware sees; call `rudderstackai.Elicit(ctx, req.Session, params)` instead to record that as `elicitation_action: cancel`, `error_type: invalid_request`.

## MCP Events (experimental)

MCP Events is a draft extension: an agent subscribes to server updates, delivered by signed webhook. The go-sdk has no hooks for it, so with `MCPEvents: true` two helpers wrap your own subscription handling and webhook sender. Both may change with the draft.

```go
// In your subscribe handler: created or refreshed, with the granted lifetime.
rudderstackai.TrackMCPEventSubscription(ctx, server, rudderstackai.SubscriptionChange{
	EventsIdentity: rudderstackai.EventsIdentity{Token: token}, // or UserID / AccountID
	Action:         rudderstackai.SubscriptionCreated,
	SubscriptionID: subscriptionID,
	EventName:      "comment.created",
	Arguments:      filter, // only the names are sent
	DeliveryMode:   "webhook",
	TTL:            24 * time.Hour,
})

// Your webhook sender, wrapped: one mcp_event_delivered per finished delivery.
deliver := rudderstackai.WrapMCPEventSender(server, func(ctx context.Context, d rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
	status, attempts, err := postWithRetries(ctx, d) // your sender
	return rudderstackai.DeliveryOutcome{HTTPStatus: status, AttemptCount: attempts}, err
})
```

- Subscription actions: `created`, `refreshed`, `unsubscribed`, `revoked`, `expired`, `callback_failed` (with `FailureReason`). With `TTL` on `created` or `refreshed`, an `expired` change is sent when the lifetime passes without a refresh, unsubscribe or revoke in this process.
- `delivery_status` comes from `HTTPStatus` (2xx `acknowledged`, 410 and 413 `rejected`, anything else `gave_up`) unless the sender returns `DeliveryStatus`; a sender that returns an error is `gave_up`, and the error is returned unchanged. `delivery_latency_ms` (acknowledged only) runs from `OccurredAt`; `payload_size_bytes` is the payload's size, never the payload.

## Limits

- Only MCP servers are tracked, and only on `github.com/modelcontextprotocol/go-sdk`. `github.com/mark3labs/mcp-go` is not supported yet.
- The go-sdk exposes neither the server's name nor the JSON-RPC request ID to middleware; the integration reads both through reflection, tested against the pinned go-sdk version. If a future go-sdk changes them, `server_name` falls back to `unknown` and `request_id` to `unknown` rather than failing.
- `transport` is detected only for streamable HTTP; set `Options.Transport` for stdio and SSE.
- State is in memory, per wrapped server, per process: the `mcp_client_seen` windows, `call_index`, `retry_count` and `ms_since_previous_call` counters, argument hashes, tool schemas and pending `input_required` elicitations (at most 10,000 entries each). Nothing is shared between servers or processes, so each replica and each restart emits its own `mcp_client_seen` and starts its counters again. Wrap one long-lived server and serve it from every request (`mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)`); a server built per request starts with empty state every time.
- `resource_name` needs the resource list or template list from a `resources/list` or `resources/templates/list` this process answered; otherwise it is `unknown`.
