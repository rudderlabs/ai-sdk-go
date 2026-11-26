# MCP servers

Track which AI clients call your MCP server, their tools, outcomes and customers. The agents are the users. Server events never carry tool arguments, results, resource contents, prompt text or elicitation answers.

| MCP library | Package | Entry point | Protocol revisions |
| --- | --- | --- | --- |
| [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) (official) | `github.com/rudderlabs/ai-sdk-go` | `rudderstackai.WrapMCPServer` | 2025-06-18, 2025-11-25, 2026-07-28 |
| [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) | `github.com/rudderlabs/ai-sdk-go/ruddermcpgo` | `ruddermcpgo.WrapServer` | v0.55.1 to v0.x: 2025-06-18, 2025-11-25; v1.x: also 2026-07-28 |

Both send the same events with the same options, defaults and properties. Module dependencies and tested versions: [Compatibility](integrations.md#compatibility).

- [Setup](#setup)
- [Options](#options)
- [Error messages: observing the final response](#error-messages-observing-the-final-response)
- [Events](#events)
- [Protocol revisions](#protocol-revisions)
- [Identity](#identity)
- [Auth providers](#auth-providers)
- [Joining the calling agent](#joining-the-calling-agent)
- [Agent-reported context](#agent-reported-context)
- [Declared tool events](#declared-tool-events)
- [Unmet demand: request_capability](#unmet-demand-request_capability-deprecated)
- [Elicitation](#elicitation)
- [MCP Events (experimental)](#mcp-events-experimental)
- [Limits](#limits)

## Setup

```go
server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "create_audience"}, createAudience)

if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client, // analytics.Client from github.com/rudderlabs/analytics-go/v4
	Identify: func(ctx context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
		if tok == nil {
			return rudderstackai.Identity{}, nil
		}
		workspace, _ := tok.Extra["workspace_id"].(string)
		return rudderstackai.Identity{UserID: tok.UserID, AccountID: workspace}, nil
	},
	EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}},
	RequestContext: rudderstackai.RequestContextOptions{Enabled: true, Intents: []string{"debug_delivery", "build_audience"}},
	Transport:      rudderstackai.TransportStdio, // for a stdio server; streamable HTTP is detected
}); err != nil {
	log.Fatal(err)
}
```

Call once before serving: it adds a receiving middleware (`AddReceivingMiddleware`) and a sending middleware (`AddSendingMiddleware`), so it works with every transport the go-sdk offers. Rewrapping replaces options and keeps state. Invalid options return an error. `rudderstackai.Flush` reaches MCP clients; see [Flush and shutdown](configuration.md#flush-and-shutdown).

### mcp-go

```go
s := server.NewMCPServer("workspace-mcp", "1.4.0", server.WithToolCapabilities(true))
// s.AddTool ...

if err := ruddermcpgo.WrapServer(s, ruddermcpgo.Options{
	Analytics: client,
	Identify: func(ctx context.Context) (rudderstackai.Identity, error) {
		workspace, _ := ctx.Value(workspaceKey{}).(string) // set by your auth, e.g. server.WithHTTPContextFunc
		return rudderstackai.Identity{AccountID: workspace}, nil
	},
	EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}},
	Transport: rudderstackai.TransportStdio, // for a stdio server; streamable HTTP is detected
}); err != nil {
	log.Fatal(err)
}
```

`ruddermcpgo.Options` has the same fields as `rudderstackai.Options`, except that `Identify`, `ClientInfo` and `ClientInfoFallback` receive only the context and `ClassifyToolError` receives mcp-go's `*mcp.CallToolResult`: mcp-go verifies no bearer tokens, so read your own authentication from it. `WrapServer` uses mcp-go's own extension points: server hooks (`GetHooks`, or `WithHooks` when the server has none) and a tool handler middleware (`Use`). When `RequestContext.Enabled` is true, it removes `request_context` from the arguments before input validation and your handler, and adds it to the tool schemas of the `tools/list` result. With `CapabilityRequests`, it registers `request_capability` as a tool (with a schema that accepts any arguments, so input validation never rejects a capability outside the list; `tools/list` shows the full one). Call it after `NewMCPServer` and before serving.

## Error messages: observing the final response

With `CaptureContent`, a failed `mcp_tool_called`, `mcp_resource_read` or `mcp_prompt_used` can carry `error_message`: the JSON-RPC error message, or the text content of a tool result with `isError: true`, masked, capped at 200 characters and withheld if it still matches a sensitive pattern, or if the cut leaves one that the removed text had kept from matching ([fixture](../spec/fixtures/mcp-error-message-v0.1.json)). It is taken only from the final response written to the client, matched by request id, because middleware added after the wrapper (go-sdk `AddReceivingMiddleware`) or mcp-go `OnAfterCallTool` hooks can still replace the error or result. Neither library has a response hook, so you opt in by wrapping the transport output; **without it, `error_message` is left out** and everything else is unchanged.

| Library | Wrap | API |
| --- | --- | --- |
| go-sdk, `Server.Run` / `Server.Connect` with `*mcp.StdioTransport` or `*mcp.IOTransport` | the context and transport | `rudderstackai.ObserveMCPTransport(ctx context.Context, t mcp.Transport) (context.Context, mcp.Transport)` |
| mcp-go, `server.NewStreamableHTTPServer` | the HTTP handler | `ruddermcpgo.ObserveHTTP(h http.Handler) http.Handler` |
| mcp-go, `StdioServer.Listen` | the context, input and output | `ruddermcpgo.ObserveStdio(ctx context.Context, r io.Reader, w io.Writer) (context.Context, io.Reader, io.Writer)` |

```go
// go-sdk over stdio: pass the returned context to Run or Connect
ctx, t := rudderstackai.ObserveMCPTransport(ctx, &mcp.StdioTransport{})
err := server.Run(ctx, t)

// mcp-go over streamable HTTP
s := server.NewMCPServer("rudderstack-mcp", version, opts...)
if err := ruddermcpgo.WrapServer(s, ruddermcpgo.Options{Analytics: client, CaptureContent: true}); err != nil {
	log.Fatal(err)
}
http.Handle("/mcp", ruddermcpgo.ObserveHTTP(server.NewStreamableHTTPServer(s)))

// mcp-go over stdio
ctx, in, out := ruddermcpgo.ObserveStdio(ctx, os.Stdin, os.Stdout)
err := server.NewStdioServer(s).Listen(ctx, in, out)
```

Messages pass through unchanged, and the libraries' own connections and protocol handling stay in place: the go-sdk wrapper observes the transport's reader and writer, not its connection. A failed request's event, and any declared tool event after it, is held until its response is written. **The hold lasts at most 10 seconds: if no response is written by then, the event goes out without `error_message`**, as it does when the connection, the context or the HTTP request ends first. If a client reuses a request id (MCP forbids it) before the earlier response is written, the earlier event also goes out without it; later matches are not guaranteed for such a client. Cancelled requests never carry it. `ObserveHTTP` reads JSON responses and SSE `data:` frames of the request it wraps (mcp-go's legacy SSE transport answers on another stream, so it gets no `error_message`); content buffering is capped at 1 MiB, while oversized messages still acknowledge successful writes and retain protocol outcome metadata and result size. The go-sdk's `StreamableHTTPHandler` builds its transports internally, so go-sdk HTTP servers cannot opt in yet. On the go-sdk, other transports (in-memory, SSE, streamable HTTP, custom) are returned unchanged.

### Observed result capture

MCP tool results reuse the written wire message after a successful write; a failed write captures no result, and its event keeps the handler's outcome and size. Transports without a write observer keep their existing release point, as do results whose response the observer never sees (closed first, timed out or a reused request id). Handler duration and completion time stay fixed at handler completion while MCP capture waits for the write. An oversized observed MCP message keeps no body: its result is captured from the handler's value after the write, with the written result size.

## Options

| Field                | Default    | What it does                                                                                                                                                                      |
| -------------------- | ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Analytics`          | required   | The RudderStack client (`analytics.Client`).                                                                                                                                      |
| `Identify`           | none       | `func(ctx, *auth.TokenInfo) (Identity, error)`, called once per request with the verified bearer token (`nil` without one); `func(ctx) (Identity, error)` on mcp-go. Returns `UserID`, `AnonymousID`, `AccountID`, `OAuthClientID`, `TokenScopes`, `AuthMethod`, `Country`. If it fails or panics, the request is anonymous. |
| `ClientInfo`         | none       | `func(ctx, *auth.TokenInfo) (name, version string)`; `func(ctx) (name, version string)` on mcp-go. Resolves after initialize, metadata and session state, before remembered clients; panics are swallowed. |
| `ClientInfoFallback` | User-Agent | Same signature as `ClientInfo`. Resolves only after all earlier sources, including `ClientInfo`, provide no known client (remembered clients take precedence only on non-initialize requests); panics are swallowed. Without a result, HTTP requests fall back to the User-Agent. Feeds `mcp_client_seen` under the same conditions as `ClientInfo`. |
| `ClassifyToolError`  | none       | `func(toolName string, result *mcp.CallToolResult) string` using the wrapper's MCP library. Classifies tool error results; invalid values and panics fall back to status hints and validation. |
| `EventRules`         | none       | Declared tool events. Names starting with `ai_` or `mcp_` are rejected.                                                                                                          |
| `RequestContext`     | off        | `Enabled: true` injects `request_context`; `Intents` and `DisableUserSentiment` apply only when enabled.                                                                          |
| `ClientSeenWindow`   | 24 hours   | Window in which a (client, OAuth client, user) is reported once by `mcp_client_seen`, per server name and process.                                                                                               |
| `CaptureContent`     | global setting | Sends masked `call_reason` and `capability_detail` capped at 200 characters, `feedback_text` capped at 2,000, and `error_message` capped at 200 when the [final response is observed](#error-messages-observing-the-final-response). False follows `Configure`'s setting.                                                                                                |
| `ContentCapture` | none | [Per-kind capture policy](privacy.md#content-capture). |
| `CapabilityRequests` | off (nil)  | Deprecated: use `FeedbackRequests`. Existing `request_capability` behavior remains available.                                                                         |
| `FeedbackRequests` | off (nil) | `&FeedbackRequestOptions{Capabilities: []string{"bulk_export"}}` adds `submit_feedback`. Cannot be combined with `CapabilityRequests`. |
| `MCPEvents`          | off        | Enables the experimental [MCP Events](#mcp-events-experimental) helpers.                                                                                                          |
| `Transport`          | detected   | `TransportStdio` or `TransportSSE`, sent as `transport` on `mcp_client_seen`. Streamable HTTP is detected from the request headers; stdio and SSE look the same from inside the server. |
| `ServerName`         | the server's | Overrides the name the server was created with, sent as `server_name`. State is kept per server name: give each distinct server its own. |
| `Redact`             | none       | `func(text string) (string, error)`, run after the built-in masking of `call_reason`, `capability_detail` and `feedback_text`. If it fails or panics, the field is not sent. |
| `DisableRedaction`   | off        | Turns redaction off: the built-in masking and `Redact`. `call_reason`, `capability_detail`, `feedback_text` and `error_message` are still withheld when they match a built-in rule, and text over 32,000 code points is withheld unscanned ([fixture](../spec/fixtures/mcp-agent-text-v0.1.json)). |

## Events

Every event is a track call, within the 30,000-byte [properties budget](events.md), with `context.wrapperLibrary` = `{name: "ai-sdk-go", version, specVersion: "0.4.0", integration: "mcp"}` and, when known, `server_name`, `server_version`, `account_id`, `mcp_session_id`, `client_name`, `client_version`, `protocol_version`, `interaction_id` and `trace_id`. Request-scoped events, including declared tool events, also carry `oauth_client_id` and `auth_method` with the same values and omission rules as `mcp_client_seen`; they do not carry `token_scopes` or `country`. The experimental MCP Events helpers do not add these authentication fields. Schemas: [`spec/ai-events-v0.4.schema.json`](../spec/ai-events-v0.4.schema.json). Events without a user are sent with `anonymousId: "ai_system"`.

| Event                            | When                                                                   | Main properties |
| -------------------------------- | ---------------------------------------------------------------------- | --------------- |
| `mcp_client_seen`                | The first request of a (client name, OAuth client, user) in the window | `server_version`, `client_title`, `client_capabilities`, `transport`, `auth_method`, `oauth_client_id`, `token_scopes`, `country` |
| `mcp_tool_called`                | A `tools/call` completes, fails or is cancelled                        | `request_id`, `tool_name`, `tool_status`, `error_type`, `is_error_result`, `duration_ms`, `time_to_cancel_ms`, `argument_keys`, `invalid_arguments`, `retry_count`, `call_index`, `ms_since_previous_call`, `progress_requested`, `result_size_bytes`, `result_tokens_estimate`, `result_content_types`, `result_paginated`, `intent`, `user_sentiment`, `call_reason`, `caller_model`, `caller_model_source` |
| `mcp_elicitation_completed`      | The client answered (or failed to answer) a question the server asked  | `elicitation_action`, `tool_name`, `request_id`, `duration_ms`, `error_type` |
| `mcp_resource_read`              | A `resources/read` completes or fails                                  | `request_id`, `resource_name` (the registered name or URI template, never the URI), `tool_status`, `error_type`, `duration_ms`, `result_size_bytes` |
| `mcp_prompt_used`                | A `prompts/get` completes or fails                                     | `request_id`, `prompt_name`, `tool_status`, `error_type`, `duration_ms`, `result_size_bytes` |
| Declared tool events             | A call of a tool with a rule                                           | `tool_name`, `tool_status` and the rule's properties |
| `mcp_feedback_submitted` | The agent called `submit_feedback` (opt-in); refused calls send nothing | `feedback_category`, `feedback_tool_name`, `capability`, `feedback_text` |
| `mcp_capability_requested`       | Deprecated: use `FeedbackRequests`                         | `capability`, `capability_detail` |
| `mcp_event_subscription_changed` | You record an MCP Events subscription change (experimental)            | `subscription_action`, `subscription_id`, `event_name`, `argument_keys`, `delivery_mode`, `ttl_ms`, `failure_reason` |
| `mcp_event_delivered`            | A wrapped sender finishes one delivery (experimental)                  | `subscription_id`, `event_name`, `event_id`, `delivery_status`, `http_status`, `attempt_count`, `delivery_latency_ms`, `payload_size_bytes` |

How the `mcp_tool_called` signals are derived:

- **`error_message`**: sent with `CaptureContent` only when the application observes the final response; see [Error messages](#error-messages-observing-the-final-response).
- **`tool_status` and `error_type`**: a result with `IsError` is `error` with `is_error_result: true` (`mcp.AddTool` turns a handler error and an argument validation failure into one); its `error_type` is `invalid_request` when arguments failed the schema, else `api_error`. A returned error maps by JSON-RPC code: parse error, invalid request, method not found, invalid params, header mismatch (-32020), missing client capability (-32021) and unsupported protocol version (-32022) are `invalid_request`, request timeout (-32001) `timeout`, connection closed (-32000) `connection_error`, URL elicitation required (-32042) `auth_error`. Other errors map as follows: a context deadline or network timeout is `timeout`, a refused or reset connection `connection_error`, then by HTTP status (`StatusCode()` or `HTTPStatusCode()`: 429 `rate_limit`, 408 and 504 `timeout`, 401 and 403 `auth_error`, 400 and 422 `invalid_request`) or type name (`ratelimit`, `throttling`, `timeout`, `cancel`, `authentication`, `accessdenied`, `badrequest`, `connection`, ...), following wrapped errors; anything else is `api_error`. A call the client cancels (`notifications/cancelled`) is `cancelled` with `time_to_cancel_ms` instead of `duration_ms`; a call that completed before the cancellation reached it stays a success.
- **`invalid_arguments`**: on a failed call, the names of the arguments that fail the tool's input schema one level deep (missing required, wrong `type`, not in `enum`, not `const`, unexpected when `additionalProperties: false`). Needs the schema from a `tools/list` this process answered.
- **`call_index`, `ms_since_previous_call` and `retry_count`**: counted per server name and process, additionally scoped by the identified account and user (missing values are empty strings), per `mcp_session_id`, else per caller-sent `interaction_id` (only one the server keeps, see [Joining the calling agent](#joining-the-calling-agent)), else per stdio or in-memory connection. `call_index` starts at 0; `ms_since_previous_call` is the gap since the previous call in the scope ended; `retry_count` counts earlier calls of the same tool with the same arguments (a SHA-256 hash of their canonical JSON, kept in memory). Without any scope (a stateless HTTP request with a minted `interaction_id`) the three are left out.
- **Names and request IDs**: `tool_name` and `prompt_name` are the exact registered name at call time (read from the server's current registry, including mcp-go task tools and the session's own tools, never from lists clients saw), otherwise `unknown`; `client_capabilities` keeps only the MCP capability names `roots`, `sampling`, `elicitation`, `tasks`, `experimental` and `extensions` (others become `unknown`), and `protocol_version` is kept only as a `YYYY-MM-DD` date (an HTTP `Mcp-Protocol-Version` header is the fallback when the request carries none); `argument_keys` and `invalid_arguments` keep only top-level registered input properties, mapping other names to `unknown` and deduplicating in first-seen order (argument keys are sorted by Unicode code point before mapping, with at most 100 names). The injected `request_context` is excluded when enabled, and an undeclared `request_context` becomes `unknown` when disabled; subscription argument names become `unknown` because no tool schema is available, while `client_capabilities` retains its existing sorting and limits.

  `request_id` is an integer-valued id within +-9007199254740991 as a decimal string (`7.0` is `7`, `-0` is `0`), any non-empty string id is `sha256:` plus the first 16 hex characters of its SHA-256 over its UTF-8 bytes, and anything else (empty string, boolean, object, fractional or out-of-range number) `unknown`; `7` and `"7"` are different requests. The go-sdk decodes every numeric id to `int64` before middleware runs, so `1.5` reaches the integration as `1` and an out-of-range number as an out-of-range integer; that limit cannot be worked around without a transport hook, and `null` ids are notifications in both libraries (no event).

  Declared tool event properties are normalised by name (case, `_` and `-` ignored): `request_id` is always the actual request's, argument lists are cleaned as above, a returned `client_name` or `client_version` is dropped (the event carries the request's own, if any), `client_title` follows the next rule, and the canonical snake_case name is emitted.
- **Client details**: `client_name`, `client_title` and `client_version`, from any source (initialize, `_meta`, session state, `ClientInfo`, a remembered client, `ClientInfoFallback`, the User-Agent, a declared tool event's `client_title` and `SubscriptionChange.ClientName`), have control characters removed, then each run of whitespace collapsed to one space and the ends trimmed. Name and title are then masked with the built-in rules (emails, phone numbers, cards, IP addresses, IDs, keys and tokens become `[email]`, `[phone]` and so on), never your `Redact` hook and also with `DisableRedaction`; they are left out when masking withholds them (over 32,000 characters or a pattern timeout) or the masked text still matches a rule, and otherwise cut to 64 code points; a name or title whose cut leaves a pattern that the removed text had kept from matching is left out too. A version is left out unless it matches `^[0-9][0-9A-Za-z.+-]{0,31}$` and no built-in rule matches it, so `v1.2.3` or `latest` is not sent ([fixture](../spec/fixtures/mcp-client-info-v0.1.json)). Empty values are omitted. Known limits: a version-shaped value can still carry digits (`415-555-0100-1` passes the version check), and a stateless server sends a client-sent `Mcp-Session-Id` header as `mcp_session_id`.
- **`caller_model`**: the model calling the tool, as the client asserts it in the request's `_meta`. Only `model` is read, from `_meta["io.modelcontextprotocol/aiInvocation"]` (SEP-2817, draft) and then `_meta["x-codex-turn-metadata"]` (Codex CLI); nothing else in `_meta` or in those objects is read. The first key whose `model` is a non-blank string wins. After trimming Unicode White_Space, keep at most 64 characters matching `^([A-Za-z0-9][A-Za-z0-9_-]*/)?[A-Za-z0-9][A-Za-z0-9._:+-]*(@[0-9]+)?$` (at most one optional `provider/` segment, with no dots or colons in the provider). Otherwise, or for ASCII case-insensitive `arn:` prefixes or endings `.gguf`, `.ggml`, `.bin`, `.safetensors`, `.onnx`, `.pt`, `.pth`, `.ckpt`, `.tflite`, `.mlmodel`, `.mlpackage`, or when the built-in masking rules would change the value (an SSN, an `sk-` key or an IP also fits the pattern), send `unknown`, never cut or fall through to a later key. Multi-segment IDs such as `accounts/fireworks/models/x` and LiteLLM's `openrouter/anthropic/x` lose their name as `unknown`, the privacy-safe direction. Known gap: an extensionless one-slash relative path such as `models/demo` is indistinguishable from provider/model and is kept. `caller_model_source` is `client_metadata`. Both are absent when the client sends no model; the value is unverified ([fixture](../spec/fixtures/mcp-caller-model-v0.1.json)).
- **Result**: `result_size_bytes` is the UTF-8 size of the result's compact JSON (no HTML escaping), `result_tokens_estimate` that size divided by 4 (rounded down), `result_content_types` the content block types plus `structured` for `structuredContent`, `result_paginated` whether the result or its `structuredContent` carries a `nextCursor` (or `next_cursor`).

## Protocol revisions

A pending elicitation expires after an hour.

The go-sdk v1.8 and mcp-go v1.x serve both revisions; both are tested end to end with a real go-sdk client, over stdio and streamable HTTP. mcp-go v0.x serves up to 2025-11-25 only: a 2026-07-28 client falls back to `initialize` with it, and the `input_required` row below does not apply.

|                                | 2025-06-18                                                     | 2026-07-28 (stateless)                                                                                       |
| ------------------------------ | -------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| Client name, version, features | The `initialize` request                                       | Each request's `_meta`: `io.modelcontextprotocol/clientInfo`, `io.modelcontextprotocol/clientCapabilities`   |
| `protocol_version`             | Negotiated in `initialize` (else the `Mcp-Protocol-Version` header) | `io.modelcontextprotocol/protocolVersion` of each request                                              |
| `mcp_client_seen`              | At `initialize`                                                | After the first successful request in the window; a `server/discover` only when the server supports the version it asks for |
| `mcp_session_id`               | The transport's session ID (`Mcp-Session-Id` on streamable HTTP) | Absent                                                                                                     |
| Elicitation                    | The server's `elicitation/create` request and the client's response | A `tools/call` result with `inputRequests`, answered by the client's retry with `inputResponses` (matched by `requestState`, else by client, user and tool) |

The `input_required` leg of a call sends no `mcp_tool_called`; the retry that completes it does, without `retry_count`, and takes the leg's `call_index` and `ms_since_previous_call`, so reported calls have no gaps. The elicitation's `request_id` is the original request's and `duration_ms` runs from the `input_required` result to the retry. A handler that returns `InputRequests` to a 2025-06-18 client is answered by the go-sdk through `elicitation/create`, which is recorded the same way as a direct `session.Elicit`. mcp-go answers it the same way, but with no hook for it: those elicitations are not recorded.

## Identity

`Identify(ctx, token)` receives the `*auth.TokenInfo` that `auth.RequireBearerToken` verified (nil over stdio or without auth) and returns the user and workspace. `UserID` should be the OAuth subject and `AccountID` the workspace. Defaults: `UserID` from `token.UserID`, `TokenScopes` from `token.Scopes`, `AuthMethod` `oauth` with a token, else `none`. `OAuthClientID` has no default: `auth.TokenInfo` does not carry the client ID, so return it from your verifier's `Extra`. The token itself and the client's IP address are never sent; return a coarse `Country` if you derive one. If `Identify` fails or panics, the request is sent anonymously (`anonymousId: ai_system`), without the token's user ID or scopes. On mcp-go, `Identify(ctx)` has no token: return what your own authentication put in the context.

For `tools/call`, both wrappers make `rudderstackai.ToolCallFromContext(ctx)` available inside `Identify`, `ClientInfo` and `ClientInfoFallback`. It returns `(MCPToolCall, bool)` with `Name` and original JSON `Arguments`, so identify can decode a workspace argument into `AccountID`. Returned `AccountID` and `UserID` apply only to that request's events, including its elicitations and capability requests; they do not update session identity.

Client name and version resolve in order: initialize `params.clientInfo`, per-request `_meta["io.modelcontextprotocol/clientInfo"]`, session/connection state (`ClientFor`), `ClientInfo`, remembered client on non-initialize requests, `ClientInfoFallback`, then User-Agent. Blank initialize bypasses remembered clients. A known client needs a name or version after the [client details](#events) rules; title-only or stripped details fall through. Resolution works without a session or connection key. Both resolvers receive identify's context and token (context only on mcp-go); empty fields and panics fall through.
With HTTP headers available, User-Agent fallback uses the first product token: `claude-code`, `Anthropic/ClaudeAI` for `Claude/` and `Claude-User`, `Cursor`, `openai-mcp`, `codex-mcp-client` for `codex-mcp-client/` and `codex_cli_rs/`, `opencode`, `copilot-cli`, and `rmcp` for `Devin-MCP-Client/`. Only version-shaped versions are kept; Devin's version is omitted because it is not rmcp's. Other products become `other` without a version. The raw header is never sent.
`ClientInfo`, `ClientInfoFallback` and User-Agent fallback supply `mcp_client_seen` on successful initialize or successful requests carrying client/protocol metadata. They add no triggers on ordinary requests without that metadata. Initialize attribution uses its payload, falling back when it has no name or version. Remembered clients only enrich request events. Every wrapper-emitted MCP event carries `server_version` when known and omits it otherwise.

`WrapMCPServer` supports this User-Agent fallback on Streamable HTTP only: the official Go SDK v1.8.0 legacy SSE handler does not populate `RequestExtra.Header`, so even a successful SSE tool call carrying `User-Agent: Cursor/1.7.4` cannot supply fallback client labels. For legacy SSE, provide client details through initialize, per-call metadata or a resolver. `ruddermcpgo` supports the fallback on both Streamable HTTP and legacy SSE: mcp-go v0.55.1 forwards each SSE POST request's headers to its request hooks. Neither adapter reads a User-Agent on stdio or in-memory transports.

Successful initialize remembers client name and version per server name and process, including stateless HTTP and servers built per request. It keys on OAuth client ID and user ID, else anonymous ID; if both parts are empty, nothing is remembered. Latest request receipt wins, even when `Identify` finishes out of order. Only successful initialize updates remembered or session/connection client state.

Remembered clients fill missing non-initialize details after initialize params, per-request `_meta`, session/connection state and `ClientInfo`. Expiry is initialize receipt time plus the then-current `ClientSeenWindow` (default 24 hours; zero keeps that default). Reads, window changes and rewrapping do not change existing expiry. Remembering has no opt-out. At most 10,000 entries per server name are kept, evicting the least recently stored. Only name and version are stored, never protocol or capabilities. Two clients for one user and OAuth client may be attributed to whichever initialized last. Stateless endpoints still have no `mcp_session_id`.

For a tool result with `isError: true`, `ClassifyToolError` may return `rate_limit`, `timeout`, `cancelled`, `denied`, `auth_error`, `invalid_request`, `connection_error` or `api_error`. Other values, an empty return or a panic are ignored. The default reads the first numeric HTTP status from `result._meta["com.rudderstack/http_status"]`, `structuredContent.status`, then `structuredContent.statusCode`: 401 → `auth_error`, 403 → `denied`, 408/504 → `timeout`, 429 → `rate_limit`, other 4xx → `invalid_request`, 5xx → `api_error`. Without a mapped status, schema-invalid arguments use `invalid_request`, otherwise `api_error`. Schema-invalid argument names remain in `invalid_arguments` regardless of the classifier or HTTP status's chosen `error_type`. Error text is sent as `error_message` only when the [final response is observed](#error-messages-observing-the-final-response). Retry counting is unchanged: repeats count regardless of success or failure.

## Auth providers

When Clerk, WorkOS, Auth0 or Stytch protects your server, each request already carries a verified user and, when selected, an organisation. Verify the token in HTTP middleware, then map its subject to `UserID` and its organisation ID to `AccountID` in `Identify`, so MCP usage joins the rest of your product data. Never send the token itself.

Clerk issues OAuth access tokens to MCP clients as JWTs by default; `sub` is the Clerk user ID (`user_...`). Enable `user:org:read` on the OAuth application, list it in the protected resource metadata's `scopes_supported`, and have the client request it. Only then does the access token carry a top-level `org_id`: the user selects the organisation on the consent screen, or Clerk uses their last active organisation when consent is disabled. Without that scope, `AccountID` stays unset. See [Clerk's organisation scope](https://clerk.com/changelog/2026-05-14-oauth-organizations).

For JWT tokens, use `github.com/golang-jwt/jwt/v5@v5.3.1` and `github.com/MicahParks/keyfunc/v3@v3.9.2`. This handler checks the signature, expiry, issuer and audience (the MCP resource URL) before reading claims; replace `issuer` and `resource` with your URLs and pass a server and analytics client from [Setup](#setup). Clerk adds `aud` only after you add that URL under **Agents → MCP servers** in the Clerk Dashboard; until then, audience verification rejects every token.

```go
import (
	"context"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

func clerkHandler(ctx context.Context, server *mcp.Server, client analytics.Client) (http.Handler, error) {
	const issuer = "https://your-instance.clerk.accounts.dev"
	const resource = "https://mcp.example.com/mcp"
	keys, err := keyfunc.NewDefaultCtx(ctx, []string{issuer + "/.well-known/jwks.json"})
	if err != nil {
		return nil, err
	}
	verifier := func(ctx context.Context, raw string, _ *http.Request) (*auth.TokenInfo, error) {
		claims := struct {
			jwt.RegisteredClaims
			Scope    string   `json:"scope"`
			Scopes   []string `json:"scp"`
			OrgID    string   `json:"org_id"`
			ClientID string   `json:"client_id"`
		}{}
		_, err := jwt.ParseWithClaims(raw, &claims, keys.KeyfuncCtx(ctx),
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(issuer),
			jwt.WithAudience(resource), jwt.WithExpirationRequired())
		if err != nil || claims.Subject == "" {
			return nil, auth.ErrInvalidToken
		}
		if claims.Scopes == nil {
			claims.Scopes = strings.Fields(claims.Scope)
		}
		return &auth.TokenInfo{
			UserID: claims.Subject, Scopes: claims.Scopes,
			Expiration: claims.ExpiresAt.Time,
			Extra:      map[string]any{"org_id": claims.OrgID, "client_id": claims.ClientID},
		}, nil
	}
	identify := func(_ context.Context, token *auth.TokenInfo) (rudderstackai.Identity, error) {
		if token == nil {
			return rudderstackai.Identity{}, nil
		}
		orgID, _ := token.Extra["org_id"].(string)
		clientID, _ := token.Extra["client_id"].(string)
		return rudderstackai.Identity{UserID: token.UserID, AccountID: orgID, OAuthClientID: clientID}, nil
	}
	if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{Analytics: client, Identify: identify}); err != nil {
		return nil, err
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource/mcp",
	})(h), nil
}
```

`OAuthClientID` has no default; the sample carries the token's `client_id` through `Extra`.

- [WorkOS AuthKit](https://workos.com/docs/authkit/sessions): access-token `sub` is the user; `org_id` is the selected organisation, when present.
- [Auth0](https://auth0.com/docs/manage-users/organizations/using-tokens): user access-token `sub` is the user; `org_id` is present when issued for an Auth0 Organization.
- Stytch: [Connected Apps access-token](https://stytch.com/docs/api-reference/consumer/api/connected-apps/tokens/connected-app-access-token-object) `sub` is the user; with [B2B session auth](https://stytch.com/docs/api-reference/b2b/api/sessions/authenticate-jwt), map the verified session's `member_id` and `organization_id` to `UserID` and `AccountID`.
- For mcp-go, put the verified user and organisation in context with `server.WithHTTPContextFunc` and read them in `Identify(ctx)`.

## Joining the calling agent

The server reads exactly these keys from a request's `_meta`:

- `io.modelcontextprotocol/clientInfo` supplies the per-call client name and version;
- `com.rudderstack/interaction_id` becomes `interaction_id`;
- the trace-id of a valid W3C `traceparent` becomes `trace_id`, else `com.rudderstack/trace_id`. Valid means lowercase hex, a version other than `ff` and no all-zero ID; version `00` is exactly 55 characters, and only later versions may add fields after the flags ([fixture](../spec/fixtures/mcp-traceparent-v0.1.json)).

`com.rudderstack/interaction_id` and `com.rudderstack/trace_id` are used only when, trimmed of Unicode White_Space, they are a UUID (8-4-4-4-12 hex digits, any version) or 32 hex digits, in either case and not all zeros; they are sent as trimmed, case kept. Anything else counts as absent, so a client-to-server join needs a UUID or 32-hex ID. The session ID, resource names, MCP Events IDs and names, and the IDs `Identify` returns (account, user, anonymous and OAuth client IDs; trimmed and capped once, so a cap that ends in white space keeps it) are trimmed of Unicode White_Space and capped at 256 characters; short labels (`protocol_version`, `auth_method`, `country`, `delivery_mode`, `failure_reason`) are capped at 64; client name, title and version follow [Identity](#identity); token scopes are trimmed and capped at 64 characters each, blank ones dropped, at most 100 ([fixture](../spec/fixtures/mcp-identity-v0.1.json)); tool and prompt names and argument keys are the exact registered names. State keying uses the [raw caller identity and scope](#limits). No other `_meta` content is copied. Without a usable ID the server mints a random `interaction_id` per request and sends no `trace_id`. A Go agent sends `Interaction.ID()` under `com.rudderstack/interaction_id` itself to join `mcp_tool_called` to the `ai_response_received` that made the call; it is a UUID unless you set `InteractionOptions.InteractionID` to something else. No Go integration adds it.

## Agent-reported context

The server never sees the user's prompt. Request context is **off by default**. Opt in with `RequestContext: rudderstackai.RequestContextOptions{Enabled: true}` in either wrapper's options. `Intents` and `DisableUserSentiment` apply only when enabled; setting them or `CaptureContent` alone does not enable it. With request context off, no `request_context` is added to `tools/list`, and a client-sent argument is passed to the tool untouched.

When enabled, the integration adds one optional `request_context` object to each tool's input schema in `tools/list`, removes it from the arguments before the handler runs, and records it on `mcp_tool_called`:

- `intent`: from `RequestContext.Intents` (plus `other`); a value not in the list is sent as `other`. Without a list, `intent` is not offered. Avoid sensitive categories: the label alone can reveal them.
- `user_sentiment`: `negative`, `neutral` or `positive`. `DisableUserSentiment` drops it.
- `call_reason`: one sentence on why the agent made the call; offered and sent only with `CaptureContent`, masked, capped at 200 characters and withheld if it still matches a sensitive pattern, before or after the cut.

A tool that already has a `request_context` argument keeps its own and receives it unchanged. The server's tool definitions stay unchanged. Cost: a few tokens in every tool definition and a few output tokens per call, paid by whoever runs the agent.

## Declared tool events

`EventRules.Tools` maps a tool name to a `ToolEventRule{Event, Properties, When}`. `Properties(args, result)` and `When(args, result)` see the call's arguments without the injected `request_context` when enabled (unchanged when disabled or declared by the tool) and the result's `structuredContent`, else its single text block parsed as JSON. The event carries `tool_name`, `tool_status`, the common MCP fields and the rule's properties (it cannot override the integration's own). If `Properties` panics, or its output would take the event over the 30,000-byte properties budget, the event is sent without them; if `When` panics the event is not sent.

Without `When`, declared events fire for every call, including failures and successful no-ops. Idempotent write tools may return `created:false`, `skipped`, `already_*` (such as `already_exists`) or `blocked` without writing. Check the tool's result contract in `When` to count completed writes. For a tool returning `{ "created": true }` only when it creates an audience:

```go
writeRules := rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
	"create_audience": {
		Event: "audience_created",
		When: func(_ map[string]any, result any) bool {
			value, ok := result.(map[string]any)
			return ok && value["created"] == true
		},
	},
}}

// Use writeRules on every endpoint that exposes create_audience.
err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client, EventRules: writeRules,
})
```

For status-based results, allow only statuses meaning a write occurred; omit `skipped`, `already_*` and `blocked`. Reuse rules across endpoints and both MCP libraries, including stdio, authenticated and stateless HTTP. Set `EventRules` on each server: one endpoint's configuration does not configure others. Predicates control declared events; `mcp_tool_called` still records every completed call.

## Send feedback: submit_feedback

Enable `FeedbackRequests: &rudderstackai.FeedbackRequestOptions{Capabilities: []string{"bulk_export", "edit_permissions"}}` in either `rudderstackai.Options` or `ruddermcpgo.Options`. Nil leaves the tool out; an empty capabilities list is allowed and "other" is always added. Setting both `FeedbackRequests` and `CapabilityRequests` returns a configuration error at wrap time, including rewrapping.

The injected `submit_feedback` tool has title and annotations title "Send feedback". Its `readOnlyHint`, `destructiveHint` and `idempotentHint` are false. `openWorldHint` is true only with `CaptureContent`, so clients can confirm before free text leaves. It invites one call per problem when the user asks to send feedback, a capability is missing, a tool fails or its result or description is wrong; it also accepts positive feedback. Resolved retries, transient errors and the agent's own mistakes should not be reported. The owner may read it; nobody replies.

Arguments are `category` (required: `bug_report`, `feature_request`, `documentation_issue`, `access_issue`, `performance_issue`, `positive_feedback`, `other`), optional `tool_name` from the server's current tools/list (including injected tools), and optional `capability` for `feature_request` only. Unknown capabilities map to "other"; omitting capability leaves it out of the event. With `CaptureContent: true`, `feedback` is also required: plain text with at least one graphic non-space character and at most 2,000 Unicode code points. Without capture, `feedback` is absent from the schema and refused if supplied. The capture description tells agents to describe the problem in their own words without pasting customer data, payloads, credentials or personal data.

Unknown arguments, non-string values, invalid categories or tool names, capability on another category, and invalid feedback return an MCP tool error and send no event. Success replies "Sent to the server owner. Tell the user it was sent; nobody replies." The SDK sends `mcp_feedback_submitted` with the usual MCP identity fields, `feedback_category`, optional `feedback_tool_name`, and optional `capability`. With capture, `feedback_text` follows the same masking and `Redact` hook as `call_reason` / `capability_detail`, capped at 2,000 characters and withheld if masking fails or leaves sensitive content, before or after the cut. Injected calls do not also send `mcp_tool_called`. A server's own `submit_feedback` is preserved and handled normally, with no SDK feedback event.

## Unmet demand: request_capability (deprecated)

`CapabilityRequests` and the injected `request_capability` tool are deprecated; use `FeedbackRequests` with category `feature_request`. Existing behavior remains available.

The injected tool's `title` and `annotations.title` are "Request a missing capability". Its annotations set `readOnlyHint`, `destructiveHint`, `idempotentHint` and `openWorldHint` to false. Every call records an event.

With `CapabilityRequests: &rudderstackai.CapabilityRequestOptions{Capabilities: []string{"bulk_export", "edit_permissions"}}`, `tools/list` gains a `request_capability` tool (arguments `capability` and, with `CaptureContent`, `detail`) the agent calls when no tool can do what the user needs. The integration answers it itself (your server never sees the call) and sends `mcp_capability_requested` with `capability` (a listed gap, else `other`). With `CaptureContent`, an `other` request also carries `capability_detail`: masked, capped at 200 characters, and withheld if it still matches a sensitive pattern, before or after the cut. A server that defines its own `request_capability` tool keeps it, and nothing is injected.

## Elicitation

Elicitations that reach the client are recorded by the sending middleware: the action the client returned, or `cancel` with `error_type` (`timeout` when the server's context deadline passed, `cancelled` when it was cancelled, else by JSON-RPC code). `session.Elicit` rejects a client without elicitation support before anything is sent, which no middleware sees; call `rudderstackai.Elicit(ctx, req.Session, params)` instead to record that as `elicitation_action: cancel`, `error_type: invalid_request`. On mcp-go, which has no hook for requests the server sends, call `ruddermcpgo.RequestElicitation(ctx, s, request)` in place of `s.RequestElicitation`: it records the client's action, or `cancel` with an `error_type`.

## MCP Events (experimental)

MCP Events is a draft extension: an agent subscribes to server updates, delivered by signed webhook. Neither library has hooks for it, so with `MCPEvents: true` two helpers wrap your own subscription handling and webhook sender (`ruddermcpgo.TrackMCPEventSubscription` and `ruddermcpgo.WrapMCPEventSender` on mcp-go). Both may change with the draft.

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
- A subscription change needs `SubscriptionID` and `EventName`; a delivery needs `SubscriptionID` and `EventID`, and a missing `EventName` is sent as `unknown`. A record without its required IDs is dropped (one warning per process), never sent with placeholder IDs; the sender still runs and its result is returned unchanged.
- `delivery_status` comes from `HTTPStatus` (2xx `acknowledged`, 410 and 413 `rejected`, anything else `gave_up`) unless the sender returns `DeliveryStatus`; a sender that returns an error is `gave_up`, and the error is returned unchanged. `delivery_latency_ms` (acknowledged only) runs from `OccurredAt`, and is left out without it; `payload_size_bytes` is the payload's size, never the payload.

## Limits

- Only MCP servers are tracked, on the two libraries above.
- Neither library exposes the server's name to the integration, and the go-sdk not the JSON-RPC request ID either; the integration reads them through reflection, tested against the [versions above](integrations.md#compatibility) (a test fails when the go-sdk's internals change). If server-name detection breaks, set `Options.ServerName`; otherwise `server_name` falls back to `unknown`. An unavailable request ID is omitted; a broken private-ID lookup requires an SDK compatibility fix.
- `transport` is detected only for streamable HTTP; set `Options.Transport` for stdio and SSE.
- State is in memory, per server name, per process: the `mcp_client_seen` windows, remembered client names and versions, `call_index`, `retry_count` and `ms_since_previous_call` counters, argument hashes, tool schemas, resource names, pending `input_required` elicitations and MCP Events expiry timers (at most 10,000 entries each, 1,000 resource templates, 1,000 server names; the least recently used go first). Every server of one name in the process shares it, so a server built per request keeps counting, and re-wrapping keeps it while replacing the options; give each distinct server a unique name (`Options.ServerName`). Session and interaction state is keyed by the raw account, user and session or interaction. Nothing is shared between processes: each replica and each restart emits its own `mcp_client_seen` and starts its counters again. Servers built per request are released.
- `resource_name` needs the resource list or template list from a `resources/list` or `resources/templates/list` this process answered; otherwise it is `unknown`.
