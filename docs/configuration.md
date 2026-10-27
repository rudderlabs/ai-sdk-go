# Configuration

Process-wide settings, per-interaction options, identity, pricing, classification and shutdown for the AI interaction events. `WrapMCPServer` takes its own `Options` and ignores these: see [mcp.md](mcp.md#options).

- [Configure](#configure)
- [Which client is used](#which-client-is-used)
- [InteractionOptions](#interactionoptions)
- [Interaction](#interaction)
- [EndOptions](#endoptions)
- [TaskOptions](#taskoptions)
- [Context values](#context-values)
- [Identity and cookies](#identity-and-cookies)
- [trace_id](#trace_id)
- [Privacy and masking](#privacy-and-masking)
- [Pricing](#pricing)
- [Classifier and evaluator](#classifier-and-evaluator)
- [Flush and shutdown](#flush-and-shutdown)
- [Limits](#limits)
- [Errors](#errors)
- [Logging](#logging)
- [Warehouse column types](#warehouse-column-types)
- [API reference](#api-reference)

## Configure

```go
err := rudderstackai.Configure(
	rudderstackai.WithAnalytics(client), // analytics.Client from github.com/rudderlabs/analytics-go/v4
	rudderstackai.WithPrices(map[string]rudderstackai.ModelPrice{
		"gpt-4.1-mini": {TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 0.4, OutputPerMtok: 1.6, CachedInputPerMtok: rudderstackai.Ptr(0.1)}},
	}),
	rudderstackai.WithCaptureContent(false),
	rudderstackai.WithToolBreakdown(true),
)
```

`Configure` changes only the settings passed, is safe to call concurrently, and validates every option first: an invalid one returns an error and changes nothing. A nil option is skipped.

| Option                                  | Default | Meaning                                                                                                                       |
| --------------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `WithAnalytics(client)`                 | none    | Default RudderStack client. Nil clears it.                                                                                    |
| `WithCaptureContent(on)`                | off     | Send `prompt_text`, `response_text`, `action_details.feedback_text` and score `explanation`, [masked](#privacy-and-masking). |
| `WithLLMCallEvents(on)`                 | off     | Also send one [`ai_llm_response_received`](events.md#ai_llm_response_received) per model call.                                |
| `WithToolBreakdown(on)`                 | off     | Add [`tool_breakdown`](events.md#tool_breakdown) to `ai_response_received`.                                                   |
| `WithIdentityFromCookies(on)`           | on      | Read identity from RudderStack JavaScript SDK [cookies](#identity-and-cookies).                                               |
| `WithRedaction(on)`                     | on      | Built-in [masking](#privacy-and-masking) of captured text. Off sends text as given and skips the hook.                       |
| `WithRedactHook(fn)`                    | none    | `func(text string) (string, error)` run after the built-in patterns. Nil removes it.                                          |
| `WithEventRules(rules)`                 | none    | [Declared tool events](events.md#declared-tool-events). Replaces earlier rules; event names starting with `ai_` or `mcp_` are rejected. |
| `WithClassifier(*ClassifierOptions)`    | none    | [Intent classification](#classifier-and-evaluator). Nil turns it off.                                                         |
| `WithEvaluator(fn)`                     | none    | [Scoring](#classifier-and-evaluator) after each successful answer. Nil turns it off.                                          |
| `WithServerFeedback(on)`                | on      | `TrackAction` sends `ai_user_action`. Off when the browser sends its own copy (`source: "client"`).                           |
| `WithPrices(map[string]ModelPrice)`     | none    | Registers prices, as [`RegisterPrice`](#pricing) for each. Merged into the registered ones.                                   |
| `WithTraceIDFrom(fn)`                   | none    | Reads [`trace_id`](#trace_id) from the context, for example `rudderotel.TraceID` for the active OpenTelemetry span.        |
| `WithLimits(Limits)`                    | see [Limits](#limits) | Memory caps. A zero field keeps its default.                                                                  |
| `WithOnError(fn)`                       | none    | `func(err error, info ErrorInfo)` sees [errors the SDK contained](#errors), at most once per kind a minute. Nil removes it.     |
| `WithAzureDeploymentModels(map)`        | none    | Azure OpenAI deployment name to base model, used only to price calls with provider `azure.ai.openai`. Replaces the previous mapping; nil clears it; empty names or models are rejected; matched case-insensitively. |

## Which client is used

For an interaction, the first that is set: `InteractionOptions.Analytics` (or `TaskOptions.Analytics`), then `ContextValues.Analytics` of [`WithValues`](#context-values), then the client given to the integration (`rudderopenai.WithAnalytics`, `rudderanthropic.WithAnalytics`, `ruddergenai.WithAnalytics`, `rudderbedrock.WithAnalytics`), then `WithAnalytics` of `Configure`. `TrackAction` and `TrackScore` use their own `Analytics` option, then the context's, then the configured one. Without any client, events are dropped with one warning per process.

## InteractionOptions

For `StartInteraction` and `RunInteraction`. Every field is optional.

| Field                                       | Meaning                                                                                                     |
| ------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `UserID`, `AnonymousID`                     | Identity; fall back to the context, then cookies                                                            |
| `ConversationID`                            | Your chat or thread ID; falls back to the context, else a UUID is minted (every event needs one)            |
| `InteractionID`                             | Replaces the generated UUID, for example to report a resumed run under the same ID                          |
| `Feature`, `Surface`, `AccountID`, `OnBehalfOf` | Fall back to the context                                                                                |
| `Intent`                                    | Known up front; also `SetIntent`                                                                            |
| `TraceID`                                   | See [trace_id](#trace_id)                                                                                   |
| `Trigger`                                   | `TriggerUser` (default) or `TriggerSystem`                                                                  |
| `TriggerSource`, `TaskName`, `TriggerEventID`, `ParentInteractionID` | Background and chaining fields; [`RunTask`](#taskoptions) sets them for you          |
| `Analytics`                                 | RudderStack client for this interaction                                                                     |
| `CaptureContent`, `ToolBreakdown`, `IdentityFromCookies` | `*bool` overrides of the global settings                                                       |
| `Classify`                                  | `*bool`; default on for user-triggered interactions, off for system ones                                    |
| `Context`                                   | Extra message context for every event of the interaction; replaces the context's `Context`                 |
| `Request`, `Cookies`, `CookieValues`        | Cookie sources for [identity](#identity-and-cookies)                                                        |
| `InputMethod`, `AttachmentCount`, `AttachmentTypes` | Prompt event fields                                                                                 |
| `PromptText`                                | Sent as `prompt_text` only with content capture on; given to the classifier and evaluator                   |
| `NoPromptEvent`                             | Skip `ai_user_prompt_created`                                                                               |

`RunInteraction(ctx, o, fn)` also puts the options' identity, conversation, feature, surface, account, client and context into the context `fn` receives (as `WithValues` would), so `TrackAction`, `TrackScore` and nested tasks inside it inherit them.

## Interaction

`StartInteraction` returns a context carrying the interaction and the `*Interaction`; pass the context to every model call. `FromContext(ctx)` returns the interaction a context carries, or nil. All methods are safe for concurrent use and never panic.

| Method                          | Meaning                                                                                                   |
| ------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `ID()`, `ConversationID()`      | The IDs to return to your client for `TrackAction` / `TrackScore`                                         |
| `Trigger()`, `Ended()`          | Who started it; whether `ai_response_received` was sent                                                   |
| `SetIntent`, `SetOutcome`, `SetTraceID` | Set before the end; an intent set before the first model call also lands on the prompt event      |
| `RecordCall(LLMCall) bool`      | Report a model call; false when ignored (a repeated provider response ID, or the interaction ended)      |
| `RecordToolCalls(...ToolCall)`  | Report tool calls; a call already recorded under the same `ID` is merged, not counted again              |
| `RecordToolCallCount(n)`        | Count `n` tool calls without IDs or names                                                                 |
| `RecordToolResult(id, ToolResult)` | Settle a recorded tool call: error, error type, duration, output                                       |
| `End(EndOptions)`               | Send `ai_response_received` once; later calls do nothing                                                  |

Field reference for `LLMCall`, `ToolCall` and `ToolResult`: [integrations.md](integrations.md#manual).

## EndOptions

| Field                | Meaning                                                                                                      |
| -------------------- | ------------------------------------------------------------------------------------------------------------ |
| `Status`             | Final status; derived from the calls when empty ([rules](events.md#status)). An invalid value logs a warning and is ignored. |
| `ErrorType`          | `error_type` for a non-success status                                                                        |
| `PauseReason`        | With `StatusPaused`: `PauseApproval`, `PauseHumanInput`, `PauseDeferredTool`, `PauseAuthRequired` or `PauseOther` (default) |
| `ClientDisconnected` | Sends `client_disconnected: true`; [`Middleware`](#context-values) also sets it when the request is cancelled before the handler returns |
| `ResponseText`       | Sent as `response_text` only with content capture on; given to the classifier and evaluator                 |
| `ExtraProperties`    | Your properties for `ai_response_received`; SDK-owned names are ignored                                      |

## TaskOptions

`RunTask(ctx, taskName, TaskOptions, fn)` runs background work (a schedule, webhook, queue message, Slack mention or another agent) as one system-triggered interaction named `taskName`.

- The actor is the system: `UserID` and `AnonymousID` come only from the options, never from the context or cookies, so events without them use `anonymousId: "ai_system"`. Put the customer in `AccountID` and the human the work is for in `OnBehalfOf`; both, and `Feature`, `Surface`, `Analytics` and `Context`, still fall back to the context. Set a stable, scoped `ConversationID` for bots and jobs, or each run gets a new one.
- Chaining: inside another interaction or task, `ParentInteractionID` and `ConversationID` default to it and `TriggerSource` to `agent`. Pass `ParentInteractionID` to link a run started elsewhere, or `NoParent: true` for none.
- No prompt event. `Classify` is off unless set.
- `Flush` (`*bool`): when the task ends, wait for its classification and score and flush its client, as [`Flush`](#flush-and-shutdown) does. Default on, except for a task chained to the interaction in its context.
- `FlushTimeout` (`time.Duration`): bounds that wait and flush together. Zero: 2 seconds.
- Other fields (`InteractionID`, `TraceID`, `TriggerEventID`, `CaptureContent`, `ToolBreakdown`) are as on `InteractionOptions`. An error returned by `fn` fails the answer and is returned unchanged.

## Context values

`WithValues(ctx, ContextValues{...})` sets request-wide defaults for the interactions, integration calls, `TrackAction` and `TrackScore` made with the returned context. Empty fields inherit from an enclosing `WithValues`.

```go
func handler(w http.ResponseWriter, r *http.Request) {
	ctx := rudderstackai.WithValues(r.Context(), rudderstackai.ContextValues{
		UserID:    currentUser(r).ID,
		AccountID: currentUser(r).WorkspaceID,
		Surface:   rudderstackai.SurfaceWeb,
		Request:   r, // RudderStack JavaScript SDK cookies
	})
	// RunInteraction, TrackAction, ... with ctx
}
```

Fields: `UserID`, `AnonymousID`, `ConversationID`, `Feature`, `AccountID`, `OnBehalfOf`, `Surface`, `Analytics`, `Context` (extra message context such as `{"app": {"build": "7"}}`), and the cookie sources `Request`, `Cookies`, `CookieValues`. A model call made with a context that carries identity but no interaction is reported as its own user-triggered answer; without identity it is system-triggered.

`Middleware(next http.Handler) http.Handler` does the cookie part for every request: handlers get a context as from `WithValues(r.Context(), ContextValues{Request: r})`, and an interaction that ends while the client is gone (the request context is cancelled before the handler returns) is sent with `client_disconnected: true`. An interaction that ends after the handler returned is never marked.

```go
http.Handle("/chat", rudderstackai.Middleware(http.HandlerFunc(chat)))
```

## Identity and cookies

Precedence for `userId` and `anonymousId`: the options of the call (`InteractionOptions`, `TrackActionOptions`, `TrackScoreOptions`), then `WithValues`, then cookies. When the browser runs the RudderStack JavaScript SDK (v3), its cookies join server-side AI events to the same user journey:

| Cookie            | Becomes             | Rule                                                                               |
| ----------------- | ------------------- | ---------------------------------------------------------------------------------- |
| `rl_anonymous_id` | `anonymousId`       | Unless an `AnonymousID` is set                                                     |
| `rl_session`      | `context.sessionId` | Session ID only, none once the session expired; a `sessionId` in your context wins |
| `rl_user_id`      | `userId`            | Fallback only: any explicit `UserID` or `AnonymousID` wins                         |

- Cookie sources, in order: `CookieValues` (a name-to-raw-value map), `Cookies` (a raw `Cookie` header), `Request` (an `*http.Request`); those of the options win over the context's. Use `Cookies` or `CookieValues` for queues and WebSockets.
- Plain JSON and `RS_ENC_v3_`-encoded values are read; unreadable cookies are skipped. Only these three cookies are read; the request and your other cookies are never kept.
- `rl_user_id` stays until the browser SDK's `reset()` runs. If users log out server-side without it, the next person on that device is attributed to the previous user: call `reset()` on logout, or pass `IdentityFromCookies: rudderstackai.Ptr(false)` or an explicit `AnonymousID` for signed-out requests.
- `WithIdentityFromCookies(false)` turns cookies off globally; `InteractionOptions.IdentityFromCookies` per interaction. Tasks never read cookies.
- Keep traits in RudderStack `identify` calls: the `userId` on every AI event joins it to that profile.

## trace_id

`ai_response_received` carries `trace_id` from `InteractionOptions.TraceID` or `SetTraceID`; a value you set always wins. Use it to join an answer to a Langfuse, LangSmith or OpenTelemetry trace.

The core module does not depend on OpenTelemetry. To take the trace ID of the active OpenTelemetry span, install the optional `rudderotel` module and register it:

```sh
go get github.com/rudderlabs/ai-sdk-go/rudderotel
```

```go
err := rudderstackai.Configure(rudderstackai.WithTraceIDFrom(rudderotel.TraceID))
```

`WithTraceIDFrom(fn func(context.Context) string)` is consulted with the context given to `StartInteraction` / `RunInteraction`, then with the context of each integration call until it returns a non-empty ID. `rudderotel.TraceID(ctx)` returns the trace ID of the span in `ctx`, or "". No span is created or exported. Without `WithTraceIDFrom`, `trace_id` comes only from `TraceID` / `SetTraceID`.

## Privacy and masking

Sent by default: IDs, models, token counts, latency, cost, status, tool names. Never sent by default: prompt, answer and feedback text, score explanations, tool arguments and results.

With content capture on (`WithCaptureContent(true)`, or `CaptureContent` per interaction or task), `prompt_text`, `response_text`, `action_details.feedback_text` and `explanation` are sent. They, and the classifier's `intent_summary`, are masked first:

| Pattern                                                                                                                                                                                  | Becomes    |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| Email addresses                                                                                                                                                                          | `[email]`  |
| Phone numbers: `+` and 9-15 digits, or 10-12 digits in common groupings                                                                                                                  | `[phone]`  |
| Payment card numbers that pass the Luhn check                                                                                                                                            | `[card]`   |
| IPv4 and IPv6 addresses                                                                                                                                                                  | `[ip]`     |
| API keys and tokens (`sk-`, Stripe `sk_live_` / `rk_live_`, Google `AIza`, Slack, GitHub, AWS access keys, JWTs, `Bearer`), passwords in connection URLs, values of `api_key=`, `token=`, `password=` and similar | `[secret]` |
| US Social Security numbers                                                                                                                                                               | `[ssn]`    |
| IBANs (checksum-validated)                                                                                                                                                               | `[iban]`   |

```go
err := rudderstackai.Configure(
	rudderstackai.WithCaptureContent(true),
	rudderstackai.WithRedactHook(func(text string) (string, error) {
		return honorifics.ReplaceAllString(text, "[name]"), nil // runs after the built-in patterns
	}),
)
```

- Patterns catch fixed shapes only: names, postal addresses and free-form IDs are not caught, and some order numbers look like phone numbers. Use the hook, or leave capture off.
- Fail closed: a hook that returns an error or panics drops the field. Text over 32,000 code points is withheld rather than masked.
- One rule: `WithRedaction(false)` sends captured text unmasked and does not run the hook; MCP `Options.DisableRedaction` does the same for `Redact`. The patterns are shared with the Node.js and Python SDKs ([`spec/fixtures/redaction-v0.1.json`](../spec/fixtures/redaction-v0.1.json)).

## Pricing

No prices ship with the SDK: list prices change and many customers negotiate their own. Register the models you use; unpriced calls keep their tokens and have no cost ([rules](events.md#cost)).

```go
err := rudderstackai.RegisterPrice("claude-sonnet-4-5", rudderstackai.ModelPrice{
	TokenPrice: rudderstackai.TokenPrice{
		InputPerMtok:        3,
		OutputPerMtok:       15,
		CachedInputPerMtok:  rudderstackai.Ptr(0.3),
		CacheWritePerMtok:   rudderstackai.Ptr(3.75),
		CacheWrite1hPerMtok: rudderstackai.Ptr(6.0),
	},
	TierMultipliers: map[string]float64{"batch": 0.5},
	LongContext: &rudderstackai.LongContextPrice{
		ThresholdTokens: 200_000,
		TokenPrice:      rudderstackai.TokenPrice{InputPerMtok: 6, OutputPerMtok: 22.5},
	},
	ServerToolPerCall: map[string]float64{"web_search": 0.01},
})
```

All rates are USD per million tokens and must be finite and non-negative; `RegisterPrice` returns an error otherwise and for an unknown tier. `WithPrices` registers several at once; `ClearPrices()` empties the table (the Azure mapping stays).

- Token rates (`TokenPrice`): uncached input at `InputPerMtok`, cache reads at `CachedInputPerMtok`, cache writes at `CacheWritePerMtok`, Anthropic 1-hour cache writes at `CacheWrite1hPerMtok`. A nil cache rate falls back to `InputPerMtok` (1-hour writes to `CacheWritePerMtok` first).
- `TierMultipliers`: a multiplier of the token cost per normalised service tier (`batch`, `flex`, `standard`, `fast`, `other`); a tier without one costs 1x.
- `LongContext`: a call with more input tokens than `ThresholdTokens` prices every token at these rates; its cache rates fall back to its own `InputPerMtok`; the tier multiplier still applies.
- `ServerToolPerCall`: USD per provider-hosted tool call by kind (`web_search`, `web_fetch`, `code_interpreter`, ...), not multiplied by the tier. Fees are added to the call's `cost_usd` and sent as `server_tool_cost_usd` only when every kind the call used has a fee; otherwise the call is priced without fees.
- `Provider`: limits the price to calls from that provider; it beats a generic price for the same model regardless of registration order.

Model matching, case-insensitive:

- A registered name prices that exact model, the same model with a version suffix (`-2024-08-06`, `-20250929`, `-v1`, `-latest`, `:0`, `@20250929`), and the name after a vendor prefix (`openai/gpt-4o`). Never another model of the family: `gpt-4o` does not price `gpt-4o-mini`. The longest matching name wins.
- Provider `aws.bedrock`: the model ID is tried as is, then without an inference-profile prefix (`us.`, `us-gov.`, `eu.`, `apac.`, `jp.`, `au.`, `global.`), then without the vendor prefix. `us.anthropic.claude-sonnet-4-5-20250929-v1:0` is priced by `claude-sonnet-4-5`, `amazon.nova-pro-v1:0` by `nova-pro`.
- Vertex AI model IDs such as `claude-sonnet-4-5@20250929` match `claude-sonnet-4-5` (an `@` revision is a version suffix).
- Google: the Gemini API (provider `gcp.gemini`) and Vertex AI (`gcp.vertex_ai`, also Anthropic models on Vertex AI) are priced separately. A price with `Provider: "gcp.gemini"` prices Gemini API calls only, one with `Provider: "gcp.vertex_ai"` Vertex AI calls only; a price without `Provider` prices both. The earlier labels still work as aliases: `google` shares prices with `gcp.gemini`, `google.vertex` with `gcp.vertex_ai`.
- Azure OpenAI deployment names are your own: map them with `WithAzureDeploymentModels(map[string]string{"support-prod": "gpt-4o"})` and send the provider label `azure.ai.openai` (`rudderopenai.WithProvider("azure.ai.openai")`). Events keep the deployment name; an unmapped deployment is unpriced.

`EstimateCost(provider, model, input, output, cached, cacheWrite, opts ...CostOptions)` prices one call from the registered table as a recorded call is priced; the optional `CostOptions{CacheWrite1hTokens, ServiceTier, ServerToolCalls}` adds 1-hour cache writes, the tier multiplier and server-tool fees:

```go
cost, ok := rudderstackai.EstimateCost("anthropic", "claude-sonnet-4-5", 12_000, 800, 0, 4_000,
	rudderstackai.CostOptions{CacheWrite1hTokens: 4_000, ServiceTier: "batch", ServerToolCalls: map[string]int{"web_search": 2}})
```

`ReportedCost(v)` reads a provider-reported cost (a number or decimal string) for `LLMCall.CostUSD`.

## Classifier and evaluator

Both run after a successful answer is out, never block it, share one queue (16 at once, 1,000 waiting) and run with an untracked context: their own model calls send no events, also through a tracked client.

```go
err := rudderstackai.Configure(rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{
	Client:  rudderanthropic.ClassifierClient(anthropic.NewClient(), "claude-haiku-4-5"), // or rudderopenai.ClassifierClient(openai.NewClient(), "gpt-4.1-mini")
	Intents: []string{"refund_request", "order_status", "bug_report"},
}))
```

| `ClassifierOptions` | Meaning                                                                                                            |
| ------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `Client`            | A `ClassifierClient` (`Classify(ctx, system, user) (ClassifierReply, error)`); requires `Intents`                   |
| `Classify`          | Your own `ClassifyFunc(ctx, ClassifyTurn) (ClassifyResult, error)` instead of `Client`; exactly one of the two      |
| `Intents`           | Closed list; anything else is `other`, so `other` is always possible. Required with `Client`                       |
| `Outcomes`          | Closed list; anything else is `unclear`, so `unclear` is always possible. Default `DefaultOutcomes`                |
| `IntentSummary`     | With `Client`: also ask for and send a one-sentence, masked `intent_summary`                                       |
| `Timeout`           | Per classification; zero means 5 seconds                                                                           |

`rudderopenai.ClassifierClient(client openai.Client, model string)` sends a Chat Completions request in JSON mode, `rudderanthropic.ClassifierClient(client anthropic.Client, model string)` a Messages request; both cap the reply at 300 tokens and disable retries. For another provider, implement `ClassifierClient` yourself: the SDK passes the system and user prompts; return the reply text plus `Model`, `Provider` and usage (`UsageKnown`, tokens) so `classifier_cost_usd` can be priced. `rudderstackai.ClassifierPrompt(turn, intentSummary)` builds the same prompts for a `Classify` function that calls a model itself. Event and input details: [events.md](events.md#ai_interaction_classified).

```go
err := rudderstackai.Configure(rudderstackai.WithEvaluator(func(ctx context.Context, turn rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
	v, err := judge(ctx, turn.Prompt, turn.Answer) // your own model call: never tracked
	if err != nil {
		return nil, err
	}
	return []rudderstackai.Score{{EvaluationName: "groundedness", ScoreValue: &v, Evaluator: rudderstackai.EvaluatorModel}}, nil
}))
```

The evaluator gets an `EvaluationTurn` (`InteractionID`, `ConversationID`, `Prompt`, `Answer`, `ToolNames`, `History`), has 5 seconds, and each returned score becomes one [`ai_interaction_scored`](events.md#ai_interaction_scored). Prompt and answer text are kept in memory for these only while a classifier or evaluator is configured.

## Flush and shutdown

The SDK installs no exit or signal hooks (Go has no reliable exit hook): call these yourself before the process exits.

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
rudderstackai.Flush(ctx)    // or Shutdown(ctx) when the process is stopping
client.Close()              // analytics-go: stop the client; the SDK never closes it
```

- `Flush(ctx) bool` waits for pending classifications and scores and for failed provider attempts still waiting on a retry ([Retries](integrations.md#retries)), then flushes every RudderStack client the SDK has used, in parallel, all within one deadline: `ctx`'s, or 10 seconds when it has none. A client implementing `ContextFlusher` (`Flush(ctx) error`, analytics-go v4.4.0 and later) sends its whole queue; one implementing `Flusher` (`Flush() error`) is flushed as before; a client with neither (a custom client) has no flush, is reported once as `client_outdated` and sends its queue on `Close()`. It returns false, and reports `flush_timeout`, when the deadline passed first. Because it drains your client's queue, your own `client.Flush` afterwards has nothing left to fail on: when your client's flush fails, `Flush` and `Shutdown` return false and pass your client's error, unchanged, to `WithOnError` as `flush_failed` (also when it fails after the deadline), so handle flush errors there. The `RunTask` flush reports it the same way and returns the task's own error unchanged. It never ends open interactions. `FlushTimeout(d)` is the deprecated duration form.
- `Shutdown(ctx) bool` stops queueing new classifications and scores, ends every open interaction as `response_status: incomplete` with `end_reason: shutdown`, then flushes as above. Calling it again is safe. Call it from your SIGTERM handling.
- A top-level `RunTask` waits for its own classification and score and flushes its client, at most `TaskOptions.FlushTimeout` (default 2 seconds) for both; running out is reported as `flush_timeout`, and the client's own flush error as `flush_failed`.
- Serverless: on Lambda, `Flush` before returning with a short client `Interval`, or `Close` per invocation; on Cloud Run, `Shutdown` on SIGTERM.
- Events are queued in analytics-go, so delivery errors are its own: handle them with its `Config.Callback`.

## Limits

`WithLimits(Limits{...})` bounds what the SDK holds in memory, whatever the traffic. Defaults are identical in Node.js and Python; reaching a cap is reported as `limit_reached` ([errors](#errors)).

| Field                    | Default  | At the cap                                                                                                                        |
| ------------------------ | -------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `MaxOpenInteractions`    | 10,000   | New interactions are not tracked: your code runs as usual, the open ones keep their data.                                         |
| `InteractionTTL`         | 1 hour   | An interaction open longer (never ended) is sent with `response_status: incomplete` and `end_reason: ttl_expired`, checked as interactions start and end (no timer). |
| `MaxCallsPerInteraction` | 1,000    | Model calls and tool calls past it still count in the totals and `usage_breakdown`; their per-call detail is not kept (no `tool_breakdown` entry or declared tool event). |
| `MaxMapEntries`          | 10,000   | Remembered clients, failed attempts held for a retry, each MCP map per server and mcp-go sessions: the oldest entry goes first (a held attempt is recorded at once). |

Captured text is masked with a 100 ms budget per pattern; a text that runs out of it is sent as `[redaction timed out]`, never unmasked (`redaction_timeout`). Events are sent outside the SDK's locks: when analytics-go's queue is full, only the goroutine sending waits.

## Errors

Tracking never panics into your code: a panic in the SDK, in an integration (including the AWS SDK's stream-decoding goroutine) or in a hook you configured is contained, and your call's own result or error is returned unchanged. Event values are encoded before they reach the RudderStack client, so a `MarshalJSON` that panics or fails drops that property, never the process.

Each contained error has an `ErrorInfo{Kind, Where}`. Kinds, identical in Node.js and Python:

| Kind                | Meaning                                                                                              |
| ------------------- | ---------------------------------------------------------------------------------------------------- |
| `internal`          | A panic in the SDK or an integration.                                                                |
| `user_callback`     | A hook you configured (redact, identify, event rules, classifier, evaluator, trace ID, `MarshalJSON`) failed or panicked; its documented fallback was used. |
| `limit_reached`     | A memory cap was hit.                                                                                |
| `redaction_timeout` | A masking pattern ran out of time; the field was sent as `[redaction timed out]`.                    |
| `flush_timeout`     | `Flush`, `Shutdown` or a `RunTask` flush hit its deadline.                                           |
| `flush_failed`      | Your RudderStack client's own Flush failed; the error passed is your client's, unchanged.            |
| `client_outdated`   | The RudderStack client cannot do what the SDK asked of it.                                           |

`WithOnError(fn)` gets the error and its `ErrorInfo`, at most once per kind a minute; a panic in `fn` is swallowed. Without it, each kind is logged once as a warning with the error's type, never its text (it may carry credentials).

## Logging

Warnings (a missing client, a failed `Enqueue`, an invalid status or surface, a dropped `TrackAction` / `TrackScore`, the first classifier failure) go to the default `log/slog` logger, prefixed `[rudderstack-ai]`, mostly once per process. Everything tracking drops or ignores is logged at debug level, errors by type only, never event payloads or captured text. Set `RUDDERSTACK_AI_DEBUG=1` (any non-empty value, read on every log call, as in Node.js and Python) to write it through the default slog handler whatever its level, or `slog.SetLogLoggerLevel(slog.LevelDebug)` to show it.

## Warehouse column types

RudderStack warehouse destinations create a column with the type of the first value they load and never change it: a whole number such as a free model's `0` cost makes an integer column, and later decimal costs in it are truncated. The SDK never sends a placeholder `0` for an unknown cost.

1. Validate with the tracking plan in [`spec/rudderstack-catalog/`](../spec/rudderstack-catalog), which types costs as decimal numbers.
2. Fix a cost column that is already an integer (`total_cost_usd`, `cost_usd`, `classifier_cost_usd`): PostgreSQL `ALTER COLUMN ... TYPE double precision`, BigQuery `SET DATA TYPE NUMERIC`, ClickHouse `MODIFY COLUMN ... Nullable(Float64)`; Snowflake, Redshift and Databricks need a new `DOUBLE` column, a copy, and a rename. Costs already loaded lost their decimals.

## API reference

| Package                                          | Exports                                                                                                                                                                                                 |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `github.com/rudderlabs/ai-sdk-go` (`rudderstackai`) | `Configure` and the `With*` options, `StartInteraction`, `RunInteraction`, `Interaction`, `RunTask`, `WithValues`, `Middleware`, `FromContext`, `TrackAction`, `TrackScore`, `Flush`, `FlushTimeout`, `Shutdown`, `Flusher`, `ContextFlusher`, `ErrorInfo`, the `Kind*` constants, `Limits`, `RegisterPrice`, `ClearPrices`, `EstimateCost`, `CostOptions`, `ReportedCost`, `ErrorTypeOf`, `ErrorTypeForStatus`, `ClassifierPrompt`, `DefaultOutcomes`, `Ptr`, `Version`, `LibraryName`, `SpecVersion`, the `Integration*`, `Status*`, `Pause*`, `Surface*`, `Action*`, `Evaluator*` and `Trigger*` constants; MCP: `WrapMCPServer`, `Elicit`, `TrackMCPEventSubscription`, `WrapMCPEventSender` |
| `.../rudderopenai`                               | `WithTracking`, `Middleware`, `WithProvider`, `WithAnalytics`, `WithStreamUsage`, `ClassifierClient`                                                                                                    |
| `.../rudderanthropic`                            | `WithTracking`, `Middleware`, `WithProvider`, `WithAnalytics`, `ClassifierClient`                                                                                                                       |
| `.../ruddergenai`                                | `HTTPClient`, `Transport`, `WithProvider`, `WithAnalytics`                                                                                                                                              |
| `.../rudderbedrock`                              | `WithTracking`, `WithAnalytics`                                                                                                                                                                         |
| `.../rudderotel`                                 | `TraceID`                                                                                                                                                                                               |
| `.../ruddermcpgo` (in the root module)           | `WrapServer`, `Options`, `RequestElicitation`, `TrackMCPEventSubscription`, `WrapMCPEventSender`                                                                                                        |

The root module (Go 1.26.10 or later) holds the core, the `net/http` middleware and MCP support (`ruddermcpgo` included). Each provider integration and `rudderotel` is a separate module you `go get` on its own, so MCP-only services do not pull provider SDKs. Minimum versions per module: [README](../README.md#compatibility).
