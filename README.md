# RudderStack AI SDK for Go

Standard AI product events for Go services, sent through the RudderStack Go client you already use and following the [AI events spec v0.3](spec/ai-events-v0.3.schema.json):

- AI interactions in chat apps and background agents: each answer becomes a small set of track events (prompt, answer with models, tokens, cost, latency and status, feedback, scores), however many model calls it took.
- MCP servers: each request an AI client makes becomes a track event that says which client called, which tool, whether it worked, and for which customer.

## How it fits

```
your app ─ provider client + integration ─ RunInteraction ─ analytics-go ─ RudderStack ─ warehouse, destinations
AI client ─ MCP ─ your MCP server (go-sdk or mcp-go) ─ wrapped ─ analytics-go ─ RudderStack
```

This is product analytics, not observability: it puts AI usage next to the rest of your customer data and complements your tracing tool. It is private by default: no prompt, answer, tool argument or result text is sent unless you turn content capture on, and captured text is masked.

The Node.js ([`@rudderstack/ai`](https://github.com/rudderlabs/ai-sdk-node)) and Python ([`rudderstack-ai`](https://github.com/rudderlabs/ai-sdk-python)) SDKs send the same events with the same properties.

## Install

```sh
go get github.com/rudderlabs/ai-sdk-go github.com/rudderlabs/analytics-go/v4
go get github.com/rudderlabs/ai-sdk-go/rudderopenai # plus the integration module for each provider SDK you use
```

Go 1.26.9 or later. `go get` needs read access to the `rudderlabs/ai-sdk-go` repository: while it is not public, set `GOPRIVATE=github.com/rudderlabs/*` and give git credentials for it (for example a `~/.netrc` entry or `git config url."git@github.com:".insteadOf "https://github.com/"`).

The root module holds the interaction API, the `net/http` middleware and MCP support. It depends on analytics-go, the MCP libraries (go-sdk and mcp-go), regexp2 (redaction patterns), google/uuid, and santhosh-tekuri/jsonschema (used only by the SDK's own schema tests, so it is not compiled into your binary). Each provider integration, and the OpenTelemetry helper `rudderotel`, is its own module, so an MCP-only service does not pull provider SDKs.

## Quick start

```go
import (
	"context"

	"github.com/openai/openai-go/v3"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

client, err := analytics.NewWithConfig(writeKey, analytics.Config{DataPlaneUrl: dataPlaneURL})
if err != nil {
	return err
}
defer client.Close() // sends queued events

if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client)); err != nil {
	return err
}
llm := openai.NewClient(rudderopenai.WithTracking())

// One interaction per user turn: every model call inside it adds up into one answer.
var answer, interactionID string
err = rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
	UserID: userID, ConversationID: chatID, Feature: "support_bot",
}, func(ctx context.Context, in *rudderstackai.Interaction) error {
	completion, err := llm.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4.1-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(question)},
	})
	if err != nil {
		return err // ends the answer as failed; returned unchanged
	}
	answer, interactionID = completion.Choices[0].Message.Content, in.ID() // send the ID to your client
	return nil
})

// Later, from your feedback endpoint:
rudderstackai.TrackAction(ctx, rudderstackai.TrackActionOptions{
	ActionType: rudderstackai.ActionFeedbackGiven, InteractionID: interactionID, ConversationID: chatID, UserID: userID,
	ActionDetails: map[string]any{"feedback_type": "thumbs", "feedback_value": 1},
})
```

This sends `ai_user_prompt_created`, `ai_response_received` (models, tokens, latency, status, and cost once the model is [priced](docs/configuration.md#pricing)) and `ai_user_action`. Pass the same context to every model call: a call made with a context that carries no interaction is reported as its own answer.

## Choose your integration

| Integration                                          | Module (`go get`)                                 | Attach with                                                       | Tracks                                                                                 |
| ---------------------------------------------------- | ------------------------------------------------- | ----------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| [OpenAI](docs/integrations.md#openai)                | `github.com/rudderlabs/ai-sdk-go/rudderopenai`    | `openai.NewClient(rudderopenai.WithTracking())`                   | Chat Completions and Responses API, streamed or not; OpenAI-compatible gateways        |
| [Anthropic](docs/integrations.md#anthropic)          | `github.com/rudderlabs/ai-sdk-go/rudderanthropic` | `anthropic.NewClient(rudderanthropic.WithTracking())`             | Messages API, streamed or not, also through its Bedrock and Vertex AI clients          |
| [Google Gen AI](docs/integrations.md#google-gen-ai)  | `github.com/rudderlabs/ai-sdk-go/ruddergenai`     | `genai.ClientConfig{HTTPClient: ruddergenai.HTTPClient(nil)}`     | `GenerateContent`, `GenerateContentStream` and chats, Gemini API and Vertex AI         |
| [Amazon Bedrock](docs/integrations.md#amazon-bedrock) | `github.com/rudderlabs/ai-sdk-go/rudderbedrock`  | `bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())` | `Converse` and `ConverseStream`                                                        |
| [Manual](docs/integrations.md#manual)                | `github.com/rudderlabs/ai-sdk-go`                 | `in.RecordCall(...)`                                              | Calls and tool calls of any other client                                               |
| [MCP servers](#mcp-servers)                          | `github.com/rudderlabs/ai-sdk-go` (go-sdk), `.../ruddermcpgo` (mcp-go) | `rudderstackai.WrapMCPServer`, `ruddermcpgo.WrapServer` | Clients, tool calls, resources, prompts, elicitations                    |

Every integration records the model, tokens (input always includes cached and cache-write tokens), cost inputs, finish reason, errors and tool calls; streamed calls are recorded when the stream ends. Rules that apply to all of them:

- One interaction per user turn, not per conversation or process. Pass your chat or thread ID as `ConversationID` so turns group into a conversation. Resending the history each turn is fine: each turn reports only its own calls.
- Read streams inside the `RunInteraction` function: a stream still open when the interaction ends is recorded as a cancelled call.
- The last call decides the status: a failed final call fails the answer even if your code handled the error; a failed call followed by a successful retry is a success.
- Provider SDK retries (openai-go, anthropic-sdk-go) count as one call, priced once, as in Node.js and Python ([Retries](docs/integrations.md#retries)).

### Other clients

For any other model client, report calls into the interaction:

```go
err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID, ConversationID: chatID},
	func(ctx context.Context, in *rudderstackai.Interaction) error {
		res, err := myLLM.Complete(ctx, prompt)
		if err != nil {
			in.RecordCall(rudderstackai.LLMCall{Provider: "mistral_ai", Model: "mistral-large-latest", ErrorType: rudderstackai.ErrorTypeOf(err)})
			return err
		}
		in.RecordCall(rudderstackai.LLMCall{
			Provider: "mistral_ai", Model: res.Model,
			InputTokens: res.Usage.Input, OutputTokens: res.Usage.Output, // input includes cached tokens
		})
		return nil
	})
```

`StartInteraction` returns the context and the `*Interaction` for code that cannot use a callback; call `End(rudderstackai.EndOptions{})` when the answer is out. Every `LLMCall` and `ToolCall` field: [docs/integrations.md](docs/integrations.md#manual).

### Gateways and pricing labels

- OpenAI-compatible endpoints: `rudderopenai.WithTracking(rudderopenai.WithProvider("openrouter"))` (or `litellm`, `gateway`, `azure.ai.openai`, `vllm`, ...). A cost the gateway reports (`usage.cost`, LiteLLM's `x-litellm-response-cost`) wins over registered prices ([Gateways](docs/integrations.md#gateways-and-openai-compatible-endpoints)).
- Provider labels follow OpenTelemetry `gen_ai.provider.name`: `openai`, `anthropic`, `aws.bedrock`, `gcp.gemini` (Gemini API), `gcp.vertex_ai` (Vertex AI, also for Anthropic on Vertex AI: `rudderanthropic.WithProvider("gcp.vertex_ai")`), `azure.ai.openai`.
- Gemini API and Vertex AI are priced separately; `google` and `google.vertex` are accepted as aliases of `gcp.gemini` and `gcp.vertex_ai` in prices. Bedrock inference-profile and Vertex `@revision` model IDs match the base model's price; Azure deployments map to base models with `WithAzureDeploymentModels` ([Pricing](docs/configuration.md#pricing)).

## Identity

Every event needs `userId` or `anonymousId`. Pass them in `InteractionOptions`, or set them once per request with `WithValues(ctx, ContextValues{UserID, AccountID, ...})`; nested values inherit. Runs with neither use `anonymousId: "ai_system"`. With only a `UserID`, analytics-go fills a random `anonymousId` on every event, so it never matches the user's other events; pass a stable `AnonymousID` (or use the cookies below) when you join on it.

When the browser runs the RudderStack JavaScript SDK (v3), its cookies give server-side AI events the browser's `anonymousId` and session, so they join the same user journey. Wrap your handlers:

```go
http.Handle("/chat", rudderstackai.Middleware(http.HandlerFunc(chat))) // chat passes r.Context() on
```

`Middleware` puts the request into each handler's context (as `WithValues(r.Context(), ContextValues{Request: r})`), and an interaction that ends while the client is gone (the request context is cancelled before the handler returns) is sent with `client_disconnected: true`. Without it, pass `Request`, `Cookies` (a raw `Cookie` header) or `CookieValues` to `InteractionOptions` or `ContextValues`, for example for queues and WebSockets.

| Cookie            | Becomes             | Rule                                                                        |
| ----------------- | ------------------- | --------------------------------------------------------------------------- |
| `rl_anonymous_id` | `anonymousId`       | Unless set explicitly                                                       |
| `rl_session`      | `context.sessionId` | Session ID only; none once the session has expired                          |
| `rl_user_id`      | `userId`            | Fallback only: an explicit or context `UserID` or `AnonymousID` always wins |

- Precedence: interaction options, then `WithValues`, then cookies.
- `rl_user_id` after logout: the cookie stays until the browser SDK's `reset()` runs. Call `reset()` on logout, or pass `IdentityFromCookies: rudderstackai.Ptr(false)` or an explicit `AnonymousID` for signed-out requests. `WithIdentityFromCookies(false)` turns cookies off globally ([details](docs/configuration.md#identity-and-cookies)).

## Background agents

For LLM work with no user in front of it (a schedule, webhook, Slack mention, queue message or another agent), use `RunTask`:

```go
ctx = rudderstackai.WithValues(ctx, rudderstackai.ContextValues{AccountID: job.WorkspaceID, OnBehalfOf: job.CreatedBy})
err := rudderstackai.RunTask(ctx, "alert-triage", rudderstackai.TaskOptions{TriggerSource: "schedule", ConversationID: "alert-triage:" + job.ID},
	func(ctx context.Context, task *rudderstackai.Interaction) error {
		if _, err := claude.Messages.New(ctx, params); err != nil {
			return err
		}
		return rudderstackai.RunTask(ctx, "draft-status-update", rudderstackai.TaskOptions{}, draftUpdate) // a sub-agent, chained
	})
```

- The system is the actor: task events have no user (`anonymousId: "ai_system"`) and never inherit the person of the context or cookies. Put the customer in `AccountID` and the human the work is for in `OnBehalfOf`, and set a stable, scoped `ConversationID`.
- Chaining: a task inside another interaction or task defaults `parent_interaction_id`, `conversation_id` and `trigger_source: agent` from it. Each task sends its own `ai_response_received`.
- Flush: a top-level task waits for its classification and score (the classifier timeout plus a second, as `Flush`) and flushes its client when the client implements `Flusher`; a chained sub-task does not (`TaskOptions.Flush` overrides).
- No prompt event, and no classification unless `Classify` is set ([TaskOptions](docs/configuration.md#taskoptions)).

## MCP servers

| MCP library | Import | Wrap with | Versions | Protocol revisions |
| --- | --- | --- | --- | --- |
| [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) (official) | `github.com/rudderlabs/ai-sdk-go` | `rudderstackai.WrapMCPServer` | v1.8.0 | 2025-06-18, 2025-11-25, 2026-07-28 |
| [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) | `github.com/rudderlabs/ai-sdk-go/ruddermcpgo` | `ruddermcpgo.WrapServer` | v0.55.1 and later | v0.x: 2025-06-18, 2025-11-25; v1.x: also 2026-07-28 |

```go
server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "create_audience"}, createAudience)

err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client,
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
	RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
})
// Serve as usual: server.Run(ctx, &mcp.StdioTransport{}) or mcp.NewStreamableHTTPHandler(...).
```

- Events: `mcp_client_seen` (once per client, OAuth client and user per 24 hours), `mcp_tool_called` (status, error type, duration, argument names, invalid arguments, retries, result size), `mcp_elicitation_completed`, `mcp_resource_read`, `mcp_prompt_used` and your declared tool events. Never arguments, results, resource contents, prompt text or elicitation answers.
- Both libraries take the same options and send the same events. On mcp-go, `Identify(ctx)` reads your own authentication from the context, and `ruddermcpgo.RequestElicitation` records elicitations; on the go-sdk, `rudderstackai.Elicit` records one the client cannot answer ([mcp.md](docs/mcp.md#elicitation)).
- Protocol revisions: 2025-06-18 (client details from `initialize`, `Mcp-Session-Id` as `mcp_session_id`) and the stateless 2026-07-28 (client details from each request's `_meta`, elicitation by `input_required` and the client's retry), on mcp-go from v1.0. Streamable HTTP is detected; set `Options.Transport` for stdio and SSE.
- Joins: `com.rudderstack/interaction_id` and the W3C `traceparent` (else `com.rudderstack/trace_id`) in a request's `_meta` become `interaction_id` and `trace_id`. A Go agent sends `Interaction.ID()` itself.
- Why it was called: an optional `request_context` argument (`intent` from your list, `user_sentiment`, and `call_reason` with `CaptureContent`) is added to each tool's schema and removed before your handler runs.
- State is in memory, per server name, per process (at most 10,000 entries each, least recently used first): servers rebuilt per request keep counting; give each distinct server a unique name. Replicas and restarts each emit their own `mcp_client_seen`.
- Opt-in: `CapabilityRequests` adds a `request_capability` tool for unmet demand (`mcp_capability_requested`); `MCPEvents: true` enables the experimental MCP Events helpers (`TrackMCPEventSubscription`, `WrapMCPEventSender`).

Call `WrapMCPServer` once, before serving; it takes its own options (`Configure` does not apply). Options, every property and limits: [docs/mcp.md](docs/mcp.md).

## Product signals

### Feedback

`TrackAction(ctx, TrackActionOptions{...})` sends `ai_user_action`, typically from your feedback endpoint ([quick start](#quick-start)). Action types: `feedback_given`, `feedback_retracted`, `copied_response`, `shared`, `reported`, `regenerated`, `accepted`, `rejected`, `edited`, `clicked_through`. `InteractionID` and `ConversationID` are required (else the event is dropped with a warning); `feedback_text` in `ActionDetails` is sent only with content capture on. When the browser sends its own copy, `WithServerFeedback(false)` turns the server copy off.

### Scores

```go
rudderstackai.TrackScore(ctx, rudderstackai.TrackScoreOptions{
	Score:          rudderstackai.Score{EvaluationName: "helpfulness", ScoreLabel: "pass", Evaluator: rudderstackai.EvaluatorHuman},
	InteractionID:  interactionID,
	ConversationID: chatID,
	UserID:         userID,
})
```

`TrackScore` sends `ai_interaction_scored` for a score from elsewhere (a human review, an offline judge). To score every successful answer as it happens, configure an evaluator with `WithEvaluator`; it runs after the answer is out and never blocks it ([details](docs/configuration.md#classifier-and-evaluator)).

### Intent classification

Opt-in. After each successful answer of an interaction you opened, your classifier labels it with one of your intents and an outcome, sent as `ai_interaction_classified`. Nothing waits for it.

```go
err := rudderstackai.Configure(rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{
	Client:  rudderanthropic.ClassifierClient(anthropic.NewClient(), "claude-haiku-4-5"), // its calls are never tracked
	Intents: []string{"refund_request", "order_status", "bug_report"},
}))
// Or your own function: &rudderstackai.ClassifierOptions{Classify: func(ctx context.Context, turn rudderstackai.ClassifyTurn) (rudderstackai.ClassifyResult, error) {...}}
```

`intent` is `other` and `outcome` is `unclear` when the label is not in your lists, so both values are always possible, even with your own `Intents` and `Outcomes`. Calls outside any interaction are never classified; tasks only with `Classify` set. Input, limits and cost: [docs/events.md](docs/events.md#ai_interaction_classified).

### Declared tool events

```go
err := rudderstackai.Configure(rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
	"save_memory":   {Event: "memory_captured"},
	"create_ticket": {Event: "ticket_created", Properties: func(args map[string]any, _ any) map[string]any { return map[string]any{"priority": args["priority"]} }},
}}))
```

Each call sends one track event with `tool_name`, `tool_call_id`, `tool_status`, the interaction's IDs and whatever `Properties` returns; tool arguments and results are never sent otherwise. Names starting with `ai_` or `mcp_` are reserved ([details](docs/events.md#declared-tool-events)).

### Tool breakdown and multi-agent

`WithToolBreakdown(true)` adds `tool_breakdown` entries to `ai_response_received` (per agent, MCP server, tool and parent tool: calls, failures, measured durations). `usage_breakdown` has one entry per agent, provider, model and service tier; set `LLMCall.AgentName` / `ToolCall.AgentName` to keep agents apart ([details](docs/events.md#tool_breakdown)).

## Privacy

Sent by default: IDs, models, token counts, latency, cost, status, tool names. Never sent by default: prompt, answer and feedback text, score explanations, tool arguments and results, MCP resource contents and elicitation answers.

`WithCaptureContent(true)` (or `CaptureContent` per interaction) adds `prompt_text` (from `PromptText`), `response_text` (from `EndOptions.ResponseText`), `action_details.feedback_text` and score `explanation`. Captured text and the classifier's `intent_summary` are masked before they leave the process: emails, phone numbers, payment cards, IP addresses, US SSNs, IBANs, and API keys, tokens and passwords become `[email]`, `[phone]`, `[card]`, `[ip]`, `[ssn]`, `[iban]` and `[secret]`.

- `WithRedactHook` adds your own masking after the built-ins; a hook that fails or panics drops the field. Text over 32,000 code points is withheld rather than masked. Names and postal addresses are not caught.
- One rule: `WithRedaction(false)` turns redaction off, the built-ins and the hook alike; MCP `Options.DisableRedaction` does the same for `Redact`.
- MCP servers have their own `Options.CaptureContent`. Patterns are shared with Node.js and Python ([`spec/fixtures/redaction-v0.1.json`](spec/fixtures/redaction-v0.1.json)); details: [docs/configuration.md](docs/configuration.md#privacy-and-masking).

## Serverless and workers

analytics-go batches in the background and sends its queue on `Close()`; it has no flush method. Before a script, worker or serverless handler exits:

```go
rudderstackai.Flush(0) // wait for pending classifications, scores and held retries; false if they did not finish
client.Close()         // analytics-go: send queued events
```

`Flush(timeout)` waits at most `timeout` (zero: the classifier timeout plus a second), then calls `Flush()` on every RudderStack client the SDK has used that implements `Flusher`. A top-level `RunTask` does the same for its own work and client ([details](docs/configuration.md#flush-and-shutdown)).

## Events

| Event                                                   | When                                                                         |
| ------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `ai_user_prompt_created`                                | A user-triggered interaction starts (sent when its first model call starts)  |
| `ai_response_received`                                  | The interaction ends: status, models, tokens, cost, latency, tool counts     |
| `ai_user_action`                                        | `TrackAction`: feedback, copy, share, report, regenerate                     |
| `ai_interaction_classified`                             | After a classified answer, with `intent` and `outcome`                       |
| `ai_interaction_scored`                                 | One per score: from your evaluator, or `TrackScore`                          |
| `ai_llm_response_received`                              | One per model call, only with `WithLLMCallEvents(true)`                      |
| Your declared tool events                               | One per call of a tool listed in `WithEventRules` (or MCP `Options.EventRules`) |
| `mcp_client_seen`, `mcp_tool_called`, `mcp_elicitation_completed`, `mcp_resource_read`, `mcp_prompt_used` | MCP server requests ([mcp.md](docs/mcp.md#events)) |
| `mcp_capability_requested`                              | Opt-in: the agent called the injected `request_capability` tool              |
| `mcp_event_subscription_changed`, `mcp_event_delivered` | Experimental, opt-in: the MCP Events draft extension                         |

Every event's properties stay within 30,000 serialized bytes (RudderStack rejects events over 32 KiB): captured text goes first, then breakdown entries, and a declared tool event loses its `Properties` output, so metrics still arrive. Every event carries `context.wrapperLibrary` (`name: "ai-sdk-go"`, `version`, `specVersion: "0.3.0"`, `integration`) beside analytics-go's own `context.library`. Sample payloads, every property and error types: [docs/events.md](docs/events.md). Schema: [`spec/ai-events-v0.3.schema.json`](spec/ai-events-v0.3.schema.json); tracking plan: [`spec/rudderstack-catalog/`](spec/rudderstack-catalog).

## Configuration

`Configure(options...)` changes only the settings passed; an invalid option returns an error and changes nothing.

| Option                           | Default | Meaning                                                                                       |
| -------------------------------- | ------- | --------------------------------------------------------------------------------------------- |
| `WithAnalytics(client)`          | none    | Default RudderStack client (`analytics.Client`)                                               |
| `WithPrices(map)`                | none    | Model prices in USD per million tokens; no prices ship with the SDK (also `RegisterPrice`)    |
| `WithAzureDeploymentModels(map)` | none    | Azure OpenAI deployment name to base model, for pricing only                                  |
| `WithCaptureContent(on)`         | off     | Send prompt, answer and feedback text (masked)                                                |
| `WithRedaction(on)`              | on      | Masking of captured text; off also skips the hook                                             |
| `WithRedactHook(fn)`             | none    | Your masking, run after the built-ins                                                         |
| `WithIdentityFromCookies(on)`    | on      | Read identity from RudderStack JavaScript SDK cookies                                         |
| `WithToolBreakdown(on)`          | off     | Add `tool_breakdown` to `ai_response_received`                                                |
| `WithLLMCallEvents(on)`          | off     | Also send `ai_llm_response_received` per model call                                           |
| `WithEventRules(rules)`          | none    | Declared tool events                                                                          |
| `WithClassifier(*opts)`          | none    | Intent classification; nil turns it off                                                       |
| `WithEvaluator(fn)`              | none    | Your scoring function, run after each successful answer; nil turns it off                     |
| `WithServerFeedback(on)`         | on      | Off: `TrackAction` sends nothing (the browser sends `ai_user_action`)                         |
| `WithTraceIDFrom(fn)`            | none    | Where `trace_id` comes from, for example `rudderotel.TraceID`                                 |

Integration options: `WithProvider` and `WithAnalytics` on every integration but Bedrock (`WithAnalytics` only), and `rudderopenai.WithStreamUsage(bool)` (streamed Chat Completions ask for usage, with a resend when a host rejects it; true always asks, false never). `EstimateCost(provider, model, input, output, cached, cacheWrite, opts ...CostOptions)` prices one call from the registered table, with `CostOptions{CacheWrite1hTokens, ServiceTier, ServerToolCalls}`. Per-interaction options, client precedence, cost rules, warehouse column types and the API reference: [docs/configuration.md](docs/configuration.md).

## Advanced: trace_id from OpenTelemetry

`trace_id` joins an answer to a Langfuse, LangSmith or OpenTelemetry trace. Pass it as `InteractionOptions.TraceID` or `SetTraceID`, or read it from the active span with the optional `rudderotel` module (the core does not depend on OpenTelemetry):

```go
err := rudderstackai.Configure(rudderstackai.WithTraceIDFrom(rudderotel.TraceID)) // go get github.com/rudderlabs/ai-sdk-go/rudderotel
```

A value you set always wins. No span is created or exported ([trace_id](docs/configuration.md#trace_id)).

## Troubleshooting

| Symptom                                    | Cause and fix                                                                                                                         |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| No events, warning `no RudderStack client` | No client anywhere: `Configure(WithAnalytics(client))`.                                                                               |
| Events missing from a job or script        | The process exited before analytics-go sent its batch: `Flush(0)`, then `client.Close()`.                                             |
| One answer split into many                 | Calls ran with a context that carries no interaction, or a stream was read after the interaction ended. Pass the interaction's `ctx`. |
| No `total_cost_usd`                        | A model is not priced and no gateway reported a cost, a call failed or was cancelled, or a streamed call had no usage.                 |
| No `ai_interaction_classified`             | No classifier, a call outside an interaction, an answer that failed, or a classifier error (logged once).                             |
| Cost column is an integer in the warehouse | The first loaded value was `0`: see [warehouse column types](docs/configuration.md#warehouse-column-types).                           |

Set `RUDDERSTACK_AI_DEBUG=1` (any non-empty value turns it on; it is read on every log call) to log everything tracking drops or ignores at debug level through the default `log/slog` handler, whatever its level, prefixed `[rudderstack-ai]`: errors by type only, never event payloads or captured text. Warnings (same prefix) are logged mostly once per process without it. Tracking never panics into your code and returns provider errors unchanged.

## Compatibility

Each module requires the oldest version it supports, so adding it never upgrades your SDK. CI runs every module's tests twice: alone, at the minimums in its `go.mod`, and in the workspace, with the newer versions the examples use.

| Module                                        | Requires and tested at (minimum)                               | Also tested with |
| --------------------------------------------- | -------------------------------------------------------------- | ---------------- |
| All modules                                   | Go 1.26.9                                                      | Go 1.26, 1.27    |
| `github.com/rudderlabs/ai-sdk-go`             | `analytics-go/v4` v4.3.1, MCP `go-sdk` v1.8.0, `mcp-go` v0.55.1 | `mcp-go` v1.1.1, v1.2.1 |
| `.../rudderopenai`                            | `github.com/openai/openai-go/v3` v3.70.0                        | v3.77.0          |
| `.../rudderanthropic`                         | `github.com/anthropics/anthropic-sdk-go` v1.78.0                | v1.80.0          |
| `.../ruddergenai`                             | `google.golang.org/genai` v1.72.0                               | v1.73.0          |
| `.../rudderbedrock`                           | `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` v1.63.1   | v1.63.4          |
| `.../rudderotel`                              | `go.opentelemetry.io/otel/trace` v1.47.0                        |                  |

Every module runs the shared cross-SDK fixtures in [`spec/fixtures/`](spec/fixtures) with the Node.js and Python SDKs.

## Known limitations

- Integrations: OpenAI, Anthropic, Google Gen AI and Bedrock clients only. There are no OpenAI Agents SDK, Claude Agent SDK, LangChain, LangGraph or Vercel AI SDK integrations (they have no Go version), and no OpenTelemetry span processor or exporter.
- Streams must be read inside the interaction; there is no `bind`. `paused` is set only through `EndOptions`.
- Not tracked: OpenAI background responses that are only queued, embeddings, and every non-generation endpoint. A cancelled Chat Completions stream keeps 0 tokens (usage arrives only at the end).
- Retries: genai retries, and openai-go / anthropic-sdk-go connection-error retries outside an interaction, are recorded per attempt ([Retries](docs/integrations.md#retries)).
- Bedrock stream read-ahead can leave the last one or two decoded events of an early-closed stream uncounted.
- MCP: only servers are tracked, on go-sdk and mcp-go. Neither library exposes the server name (nor the go-sdk the JSON-RPC request ID): they are read through reflection, tested against the versions above, falling back to `unknown`. Only streamable HTTP is detected as a transport. mcp-go has no hook for elicitations a handler answers through `InputRequests` on 2025-06-18. State is in memory per server name, per process ([details](docs/mcp.md#limits)).

## Examples

| Example                                               | Shows                                                                                          |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| [`openai`](examples/openai)                           | `rudderopenai`: a streamed Chat Completions answer, a registered price, `TrackAction` feedback |
| [`anthropic`](examples/anthropic)                     | `rudderanthropic`: a streamed Messages answer with cache reads and writes                      |
| [`google-genai`](examples/google-genai)               | `ruddergenai`: a tool call, then a streamed answer, with `tool_breakdown`                      |
| [`bedrock-converse`](examples/bedrock-converse)       | `rudderbedrock`: `Converse` with a tool, then `ConverseStream`, priced through an inference profile |
| [`background-agent`](examples/background-agent)       | `RunTask`: system-triggered work with a chained sub-task and `TrackScore`                      |
| [`manual`](examples/manual)                           | `StartInteraction`, `RecordCall`, `RecordToolCalls`, a declared tool event, a custom classifier |
| [`mcp-server`](examples/mcp-server)                   | stdio and streamable HTTP, `request_context`, a declared event, `request_capability`           |
| [`mcp-server-mcpgo`](examples/mcp-server-mcpgo)       | The same server on mcp-go with `ruddermcpgo.WrapServer`, 2025-06-18 and 2026-07-28             |
| [`mcp-stateless-http`](examples/mcp-stateless-http)   | Stateless streamable HTTP on 2026-07-28, confirmation through an `input_required` elicitation  |
| [`mcp-auth`](examples/mcp-auth)                       | A bearer token check and `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |

From the repository root: `go run ./examples/<name> --check` runs one end to end against a local mock data plane and a mock LLM (no credentials); `--live` sends the same events to `RUDDERSTACK_DATA_PLANE_URL` with `RUDDERSTACK_WRITE_KEY`; without flags a provider example makes one real call and an MCP example serves. Details: [examples/README.md](examples/README.md).

## Documentation

- [docs/events.md](docs/events.md): every AI interaction event and property, status, totals, `usage_breakdown`, `tool_breakdown`, cost, error types.
- [docs/configuration.md](docs/configuration.md): `Configure` options, interaction and task options, context values and `Middleware`, cookies, `trace_id`, masking, pricing and `EstimateCost`, classifier and evaluator, flushing, logging, API reference.
- [docs/integrations.md](docs/integrations.md): per-integration setup and behaviour, streaming, retries, gateways, manual recording, limits.
- [docs/mcp.md](docs/mcp.md): MCP server tracking.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [RELEASING.md](RELEASING.md). Report security issues as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE.md)
