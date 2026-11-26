# Configuration

Settings, interactions, identity, pricing, classification and shutdown. MCP has [separate options](mcp.md#options), with global sampling and content capture.

- [Configure](#configure)
- [Which client is used](#which-client-is-used)
- [InteractionOptions](#interactionoptions)
- [Interaction](#interaction)
- [EndOptions](#endoptions)
- [TaskOptions](#taskoptions)
- [Context values](#context-values)
- [Identity and cookies](#identity-and-cookies)
- [trace_id](#trace_id)
- [Privacy and masking](privacy.md#privacy-and-masking)
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

`Configure` changes only the settings passed and is safe to call concurrently. It validates all options before changing anything. An invalid option returns an error. Nil options are skipped.

| Option                                  | Default | Meaning                                                                                                                       |
| --------------------------------------- | ------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `WithAnalytics(client)`                 | none    | Default RudderStack client. Nil clears it.                                                                                    |
| `WithCaptureContent(on)` | see [capture policy](privacy.md#content-capture) | Content capture shorthand. |
| `WithContentCapture(policy)` | none | [Per-kind capture policy](privacy.md#content-capture). |
| `WithEmitSpans(on)` | see [tracing](tracing.md#wrapper-and-manual-spans) | Wrapper and manual spans. |
| `WithLLMCallEvents(on)`                 | off     | Also send one [`ai_llm_response_received`](events.md#ai_llm_response_received) per model call.                                |
| `WithTaskStartedEvents(on)`             | off     | Also send [`ai_task_started`](events.md#ai_task_started) when a `RunTask` run starts (not when it resumes).                   |
| `WithToolBreakdown(on)`                 | off     | Add [`tool_breakdown`](events.md#tool_breakdown) to `ai_response_received`.                                                   |
| `WithIdentityFromCookies(on)`           | on      | Read identity from RudderStack JavaScript SDK [cookies](#identity-and-cookies).                                               |
| `WithRedaction(on)` | on | [Masking](privacy.md#privacy-and-masking). |
| `WithRedactHook(fn)`                    | none    | `func(text string) (string, error)` run after the built-in patterns. Nil removes it.                                          |
| `WithEventRules(rules)` | none | [Declared tool events](events.md#declared-tool-events). |
| `WithClassifier(*ClassifierOptions)`    | none    | [Intent classification](#classifier-and-evaluator). Nil turns it off.                                                         |
| `WithEvaluator(fn)`                     | none    | [Scoring](#classifier-and-evaluator) after each successful answer. Nil turns it off.                                          |
| `WithServerFeedback(on)`                | on      | [Server feedback](events.md#ai_user_action).                        |
| `WithPrices(map[string]ModelPrice)`     | none    | Registers prices, as [`RegisterPrice`](#pricing) for each. Merged into the registered ones.                                   |
| `WithTraceIDFrom(fn)` | none | [Product trace ID](tracing.md#product-trace-id). |
| `WithLimits(Limits)`                    | see [Limits](#limits) | Memory caps. A zero field keeps its default.                                                                  |
| `WithOnError(fn)`                       | none    | `func(err error, info ErrorInfo)` sees [errors the SDK contained](#errors), at most once per kind a minute. Nil removes it.     |
| `WithAzureDeploymentModels(map)`        | none    | Azure OpenAI deployment name to base model, used only to price calls with provider `azure.ai.openai`. Replaces the previous mapping; nil clears it; empty names or models are rejected; matched case-insensitively. |
| `WithSampleRates(map[string]float64)`   | none    | Share of each event to send, by event name, from 0 to 1 (see [Sampling](#sampling)). Replaces the previous rates; nil clears them; a rate outside 0 to 1 or an empty event name is rejected. |

## Which client is used

For an interaction, the first that is set: `InteractionOptions.Analytics` (or `TaskOptions.Analytics`), then `ContextValues.Analytics` of [`WithValues`](#context-values), then the client given to the integration (`rudderopenai.WithAnalytics`, `rudderanthropic.WithAnalytics`, `ruddergenai.WithAnalytics`, `rudderbedrock.WithAnalytics`), then `WithAnalytics` of `Configure`. `TrackAction` and `TrackScore` use their own `Analytics` option, then the context's, then the configured one. Without any client, events are dropped with one warning per process.

## InteractionOptions

`InteractionOptions`, `TaskOptions`, `ContextValues`, `TrackActionOptions`, `TrackScoreOptions`, and `TrackClassificationOptions` accept `AgentVersion` and `Variant`.

For `StartInteraction` and `RunInteraction`. Every field is optional.

Use one interaction per user turn. Set `ConversationID` to your chat or thread ID to group turns. For a feature without a thread, such as a Generate button or suggested fix, set `OneShot: true`. With no explicit or context conversation ID, `OneShot` uses the interaction ID as `conversation_id`. Without `OneShot`, the default generates a separate conversation ID. Resending history is fine: each turn reports only its own calls.

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
| `ContentCapture`                                         | `*ContentCapture`: [content capture](privacy.md#content-capture) per kind for this interaction            |
| `Classify`                                  | `*bool`; default on for user-triggered interactions, off for system ones                                    |
| `Context`                                   | Extra message context for every event of the interaction; replaces the context's `Context`                 |
| `Request`, `Cookies`, `CookieValues`        | Cookie sources for [identity](#identity-and-cookies)                                                        |
| `InputMethod`, `AttachmentCount`, `AttachmentTypes` | Prompt event fields                                                                                 |
| `PromptText`                                | Sent as `prompt_text` only with content capture on; given to the classifier and evaluator                   |
| `NoPromptEvent`                             | Skip `ai_user_prompt_created`                                                                               |
| `Resume`                                    | `*PausedInteraction` from [`Pause`](#pauses-and-resumes): continue that run; its IDs, identity and background fields win over the other options, and no prompt event is sent |

`RunInteraction(ctx, o, fn)` adds the options' identity, conversation, feature, surface, account, client and context to `fn`'s context, as `WithValues` does. `TrackAction`, `TrackScore` and nested tasks inherit them.

## Interaction

`StartInteraction` returns a context carrying the interaction and the `*Interaction`. Pass that context to every model call. Without a callback, call `End(rudderstackai.EndOptions{})` when the answer is out. `FromContext(ctx)` returns the interaction a context carries, or nil (the restored interaction once its snapshot was restored in this process). All methods are safe for concurrent use and never panic.

| Method                          | Meaning                                                                                                   |
| ------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `ID()`, `ConversationID()`      | The IDs to return to your client for `TrackAction` / `TrackScore`                                         |
| `Trigger()`, `Ended()`          | Who started it; whether `ai_response_received` was sent                                                   |
| `SetIntent`, `SetOutcome`, `SetTraceID` | Set before the end; an intent set before the first model call also lands on the prompt event      |
| `RecordCall(LLMCall) bool`      | Report a model call; false when ignored (a repeated provider response ID, or the interaction ended)      |
| `RecordToolCalls(...ToolCall)`  | Report tool calls; a call already recorded under the same `ID` is merged, not counted again              |
| `RecordToolCallCount(n)`        | Count `n` tool calls without IDs or names                                                                 |
| `RecordToolResult(id, ToolResult)` | Settle a recorded tool call: error, error type, duration, output                                       |
| `Pause(PauseOptions)`           | Send a `paused` `ai_response_received` for this segment and return the snapshot to resume from ([Pauses and resumes](#pauses-and-resumes)) |
| `Resume()`, `Paused()`          | Resume a paused run in this process; whether it is paused                                                 |
| `End(EndOptions)`               | Send the segment response; a paused end can resume, a final end is idempotent                                                  |

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

`RunTask(ctx, taskName, TaskOptions, fn)` runs background work (a schedule, webhook, queue message, Slack mention or another agent) as one system-triggered interaction named `taskName`.

- The actor is the system: `UserID` and `AnonymousID` come only from the options, never from the context or cookies, so events without them use the [identity fallback](#identity-and-cookies). Put the customer in `AccountID` and the human the work is for in `OnBehalfOf`; both, and `Feature`, `Surface`, `Analytics` and `Context`, still fall back to the context. Set a stable, scoped `ConversationID` for bots and jobs, or each run gets a new one.
- Chaining: inside another interaction or task, `ParentInteractionID` and `ConversationID` default to it and `TriggerSource` to `agent`. Pass `ParentInteractionID` to link a run started elsewhere, or `NoParent: true` for none.
- No prompt event. `Classify` is off unless set.
- `Flush` (`*bool`): when the task ends, wait for its classification and score and flush its client, as [`Flush`](#flush-and-shutdown) does. Default on, except for a task chained to the interaction in its context.
- `FlushTimeout` (`time.Duration`): bounds that wait and flush together. Zero: 2 seconds.
- `Resume` (`*PausedInteraction`): continue a paused run from its snapshot, without `ai_task_started` ([Pauses and resumes](#pauses-and-resumes)).
- Other fields (`InteractionID`, `TraceID`, `TriggerEventID`, `CaptureContent`, `ToolBreakdown`) are as on `InteractionOptions`. An error returned by `fn` fails the answer and is returned unchanged.

## Pauses and resumes

Snapshot outcome hints prefer canonical keys even when their value is null; legacy aliases are read only if the canonical key is absent.

A run that waits for something outside it (an approval, a person's answer) pauses: `Pause` sends `ai_response_received` with `response_status: paused`, `pause_reason` and that segment's totals, and the next segment's totals start from zero. When the run ends, its final `ai_response_received` adds `pause_count` and `total_paused_ms`. Every event keeps the same `interaction_id`.

```go
err := rudderstackai.RunTask(ctx, "refund-approval", rudderstackai.TaskOptions{TriggerSource: "queue", Resume: saved},
	func(ctx context.Context, task *rudderstackai.Interaction) error {
		if _, err := claude.Messages.New(ctx, params); err != nil {
			return err
		}
		if needsApproval {
			paused, _ := task.Pause(rudderstackai.PauseOptions{Reason: rudderstackai.PauseApproval})
			return db.SaveRun(job.ID, paused) // JSON; load it into saved on the next run
		}
		return nil
	})
```

- In process, `Resume()` continues the run; a model or tool call recorded while paused resumes it too.
- A context saved before a snapshot was restored in this process follows the run: model calls, spans and `FromContext` with it use the restored interaction, and once that ended a call is a separate answer. Snapshots produced by older SDK versions still restore but cannot link saved contexts.
- Span ownership and snapshot tenant limits: [Product events from spans](tracing.md#product-events-from-spans).
- Across processes (a worker restart, a wait of days), store the `PausedInteraction` (JSON, keys as the event properties) with your own run state and pass it as `InteractionOptions.Resume` or `TaskOptions.Resume`. The optional `last_failed_call_type` field retains the latest failed model call’s category so a resumed `RunTask` that returns a generic error reports the same error type and status. Older snapshots without it remain valid and use `app_error` until another model call fails. The snapshot carries the IDs, identity, background fields, agent version, variant, confidence, intent, outcome, trace ID, integration, pause count and paused time, and when the pause started; the time until the resume counts as paused, by the wall clock. Calls before the pause are already sent, except tool calls the pause waits on; usage accepted during pause delivery is kept for the next segment, including credits and per-call fields.
- A tool call the pause waits on, such as one awaiting approval, counts in the segment that runs it. It waits when it has a tool-call ID, no outcome yet, and its agent made no model call after it (a tool without its own `AgentName` belongs to the agent of the model call recorded before it). The paused answer leaves it out of `tool_call_count` and `tool_breakdown` and sends no declared tool event for it. When its execution or `RecordToolResult` is recorded after the resume, it counts once with its real outcome and its declared event is sent once; still unresolved at the final end, it counts there as `unknown`. A run that never resumes, or that ends while still paused, never counts it. Two costs: after a JSON restore, the `ToolEventRule` `When` and `Properties` see the tool's arguments only if the execution record repeats them, because snapshots carry no arguments or results; and an app that only uses provider wrappers (it never records tool results) and pauses after tools ran but before the next model call sees those tools counted only in the resumed answer, or never if the run is not resumed.
- A tool call counted in a paused segment (for example as `unknown`, because its agent made a later model call) and reported again after the resume counts once: the later record or `RecordToolResult` adds no count and no declared event, and the event keeps the paused segment's outcome, as a late result already does. A repeated record still resumes a paused run. Snapshots carry these IDs in `counted_tool_ids`, up to `Limits.MaxCallsPerInteraction` of them, so a restored run behaves the same; a repeat without a tool-call ID, or a call folded past that limit, still counts again.
- `RunInteraction` and `RunTask` leave a paused run paused when `fn` returns nil (an error still ends it). A paused run is off the open list: `Limits.InteractionTTL` and `Shutdown` never end it, and an abandoned one ends with its last `paused` event. A paused run counts toward `Limits.MaxOpenInteractions` until it ends or the app drops it and it is garbage-collected, which frees its slot. A restore takes over its own live pause's slot by the snapshot's `pause_token`; a snapshot without the token, or one whose paused object was collected, restores under normal admission.
- `PauseOptions`: `Reason` (default `PauseOther`), `ResponseText` (sent as `response_text` only with content capture on), `ExtraProperties`. Pausing again before a resume sends nothing and returns the same snapshot; after `End`, `ok` is false.
- `End` while paused ends the run: the wait counts as paused time, and the final segment has no calls. A tool call the pause still waits on never ran, so it is not counted and sends no event; one whose outcome was recorded while paused counts in the final segment.
- A call counted before a pause and reported again after the resume (a manual `RecordCall`, a late span) is deduplicated only when both reports carry the same response ID; otherwise it counts again, so counting is not exactly-once across a pause.

### Callback re-entry

User hooks and analytics client methods run after the SDK releases its bookkeeping locks, so a callback can re-enter tracking from another thread or goroutine.

| Callback family | Go behavior |
| --- | --- |
| Error handlers | Notices and contained build failures detached in `unlock`, reported outside `mu` |
| Redactors | Prompt/response masking outside `mu` |
| Adapter finalizers | `beforeEnd` detached under `mu`, invoked outside it |
| Tool filters/properties | Jobs copied and fired flag set under `mu`, hooks outside it |
| Transport/delivery/flush | Outbox detached before `Enqueue`; flush outside `mu` |
| Classifier/evaluator | Turn copied, work scheduled after response publication |
| JSON serialization hooks | Tool properties normalized/sized outside `mu`; delivery serialization outside `mu` |
| Identity/trace/disconnect/error classification | Startup or outside `mu` |
| Credit/pricing | `WithCredits` invokes a side-effect-free, idempotent callback outside `mu`; pricing may retry |

Interaction state publication must finish before application code runs. Pausing keeps its in-progress guard active through finalization, redaction, tool events and delivery.

Calls accepted during finalization belong to the closing segment. Calls accepted during delivery belong to the next segment. Each accepted call is emitted and counted once.

`Pause` from the same interaction's SDK callback on the same goroutine returns `ok=false` and reports `KindPauseRefused` (`pause_refused`) with `ErrorInfo.Reason: in_callback` to `WithOnError`, or one warning without it. This covers credits/retries, analytics Enqueue, redactors, content serializers and event rules. The interaction and credits stay unchanged. Pause never panics. Error handlers, application code and callbacks for other interactions may pause normally. Another goroutine waits up to 5 s for pricing, including retries/capture. Expiry refuses with `Reason: pricing_in_flight`. Late credits stay in the open segment. Callback callers use the goroutine ID from the first runtime.Stack line; failed parsing falls back to waiting.

After restoring a snapshot in the same process, run follow-up work in the restored interaction's context; helpers on a context saved before the restore keep the old client and session.

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

Fields: `UserID`, `AnonymousID`, `ConversationID`, `Feature`, `AccountID`, `OnBehalfOf`, `Surface`, `Analytics`, `Context` (extra message context such as `{"app": {"build": "7"}}`), and the cookie sources `Request`, `Cookies`, `CookieValues`. Standalone model calls: [Interaction ownership](integrations.md#interaction-ownership).

`Middleware(next http.Handler) http.Handler` gives handlers the context of `WithValues(r.Context(), ContextValues{Request: r})`. An interaction ending after request cancellation but before the handler returns carries `client_disconnected: true`. Interactions ending after the handler returns are never marked.

```go
http.Handle("/chat", rudderstackai.Middleware(http.HandlerFunc(chat)))
```

## Identity and cookies

Runs with neither `UserID` nor `AnonymousID` use `anonymousId: "ai_system"`.

With only a `UserID`, analytics-go fills a random `anonymousId` on every event, so it never matches the user's other events; pass a stable `AnonymousID` (or use the cookies below) when you join on it.

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

See [Product trace ID](tracing.md#product-trace-id).

## Pricing

`WithCredits` accepts `func(CreditCall) (*float64, error)`. It prices successful calls with known usage after USD pricing; return nil for unknown credits. `LLMCall.Credits` overrides the hook.

Credits retries preserve the newest retained call usage and metadata when a hook re-enters and matches a twin. The published credits belong to that same usage and metadata.

The credits hook must be side-effect free and idempotent. It may run again if usage or pricing metadata changes during the hook, through re-entry or concurrent enrichment. After 3 attempts, continued changes omit credits and report `credits_unstable` with `where: credits`. A failing hook instead reports `user_callback`.

Register the models you use: the SDK ships no prices, since list prices change and customers negotiate their own. Unpriced calls retain tokens and have no cost ([rules](events.md#cost)).

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

- Evaluator: runs for every interaction that ended `success`, including tasks and interactions an integration opened, after `ai_response_received`, with a 5-second timeout and an untracked context. An error, panic or timeout sends nothing.

- `intent` is one of `ClassifierOptions.Intents`, else `other` (with your own `Classify` and no `Intents`, any non-empty value is kept). `outcome` is one of `Outcomes` (default `DefaultOutcomes`: `resolved`, `partial`, `unresolved`, `escalated`, `unsupported`, `unclear`), else `unclear`. `other` and `unclear` are therefore always possible values, whatever your lists hold: plan reports and warehouse filters for them.
- Which interactions: one classification per successful interaction you open (`StartInteraction`, `RunInteraction`) with `Classify` on (the default for user-triggered ones). Tasks are off unless `TaskOptions.Classify` is set; interactions an integration opens on its own are never classified. Errors, cancellations and other non-success statuses are skipped.
- Input: the prompt (`PromptText`, else the last user message of the request messages an integration kept), the answer (`EndOptions.ResponseText`, else the last successful call's answer text), the names of the tools called (never arguments or results) and earlier messages, within a 12,000-character budget that keeps the start and end of long text. The prompt fences the turn as untrusted data and asks for JSON from closed lists ([`ClassifierPrompt`](#classifier-and-evaluator)). An interaction with neither prompt nor answer text is skipped with a warning.
- Failures: a timeout (default 5 seconds), an error, a panic or an unparseable reply sends nothing and logs one warning per process. Up to 16 classifications and evaluations run at once and up to 1,000 wait; beyond that, work is dropped with a warning at most once a minute.

Both run after a successful answer is out, never block it, and run with an untracked context: their own model calls and spans send no events, also through a tracked client. They share the queue above.

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

### Classifier clients

`rudderopenai.ClassifierClient(client openai.Client, model string)` sends a Chat Completions request in JSON mode, `rudderanthropic.ClassifierClient(client anthropic.Client, model string)` a Messages request; both cap the reply at 300 tokens and disable retries. For Gemini or Bedrock, implement `ClassifierClient` yourself. The SDK passes the system and user prompts; return the reply text plus `Model`, `Provider` and usage (`UsageKnown`, tokens) so `classifier_cost_usd` can be priced. `rudderstackai.ClassifierPrompt(turn, intentSummary)` builds the same prompts for a `Classify` function that calls a model itself. Event and input details: [events.md](events.md#ai_interaction_classified).

```go
err := rudderstackai.Configure(rudderstackai.WithEvaluator(func(ctx context.Context, turn rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
	v, err := judge(ctx, turn.Prompt, turn.Answer) // your own model call: never tracked
	if err != nil {
		return nil, err
	}
	return []rudderstackai.Score{{EvaluationName: "groundedness", ScoreValue: &v, Evaluator: rudderstackai.EvaluatorModel}}, nil
}))
```

The evaluator gets an `EvaluationTurn` (`InteractionID`, `ConversationID`, `Prompt`, `Answer`, `ToolNames`, `History`); each returned score becomes one [`ai_interaction_scored`](events.md#ai_interaction_scored). Prompt and answer text are kept in memory for these only while a classifier or evaluator is configured.

## Flush and shutdown

The SDK installs no exit or signal hooks. Call these before the process exits.

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
rudderstackai.Flush(ctx)    // send pending work while continuing to use the client
// On exit instead: end open interactions, then close each client you own.
rudderstackai.Shutdown(ctx)
rudderstackai.CloseClient(ctx, client)
```

- `Flush(ctx) bool` waits for pending classifications and scores and for failed provider attempts still waiting on a retry ([Retries](integrations.md#retries)), then flushes every RudderStack client the SDK has used, in parallel, all within one deadline: `ctx`'s, or 10 seconds when it has none. A client implementing `ContextFlusher` (`Flush(ctx) error`, analytics-go v4.4.0 and later) sends its whole queue; one implementing `Flusher` (`Flush() error`) is flushed as before; a client with neither (a custom client) has no flush, is reported once as `client_outdated` and sends its queue on `Close()`. It returns false, and reports `flush_timeout`, when the deadline passed first.

  Because it drains your client's queue, your own `client.Flush` afterwards has nothing left to fail on: when your client's flush fails, `Flush` and `Shutdown` return false and pass your client's error, unchanged, to `WithOnError` as `flush_failed` (also when it fails after the deadline), so handle flush errors there. The `RunTask` flush reports it the same way and returns the task's own error unchanged.

  It never ends open interactions. `FlushTimeout(d)` is the deprecated duration form.
- `Shutdown(ctx) bool` stops queueing new classifications and scores, ends every open interaction as `response_status: incomplete` with `end_reason: shutdown`, then calls `Flush(ctx)` and returns its result. It never closes a client: close yours afterwards. Shared clients remain available for non-AI events. Calling it again is safe. Call it from your SIGTERM handling.
- `CloseClient(ctx, client) bool` closes a client you own after `Shutdown`; call it for each client you own. A nil client returns true. It runs `Close` in a goroutine and returns true when Close finishes without error. If ctx expires, it returns false and reports `flush_timeout`; Close continues in the background. A Close error is passed unchanged as `flush_failed`, including errors returned after the deadline. Without a deadline, it waits until Close finishes or ctx is canceled.
- Task flush settings: [`TaskOptions`](#taskoptions); deadline and client errors are reported as `flush_timeout` and `flush_failed`.
- Serverless: on Lambda, `Flush` before returning with a short client `Interval`, or `Close` per invocation; on Cloud Run, `Shutdown` then `CloseClient` for your clients on SIGTERM.
- Events are queued in analytics-go, so delivery errors are its own: handle them with its `Config.Callback`.

## Sampling

`WithSampleRates` cuts the volume of high-frequency events, such as `ai_llm_response_received` and `mcp_tool_called`, at the source:

```go
err := rudderstackai.Configure(rudderstackai.WithSampleRates(map[string]float64{
	"ai_llm_response_received": 0.1,
	"mcp_tool_called":          0.05,
}))
```

- **What is kept.** An event with a rate below 1 is sent for that share of interactions. The draw is a hash of `interaction_id`, so every sampled event of one interaction is kept or dropped together, and an interaction kept at a lower rate is also kept at every higher one. An event without an `interaction_id` (`mcp_client_seen`) is drawn at random.
- **The stamp.** A kept event carries `sample_rate` (`0.1`); weight each row by `1 / sample_rate` to estimate totals. Events without a rate below 1 carry none.
- **The default.** Events you do not list are all sent. Leave `ai_response_received` out: cost totals, answer counts and the client/server gap need every answer, and its `llm_call_count`, `tool_call_count` and token totals still count the calls whose own events were sampled out. You can list it; then every metric built on it is an estimate.
- **Pauses and resumes.** Every paused and final answer is sent by default. When you sample `ai_response_received`, each segment uses the same interaction ID draw and kept segments carry the configured `sample_rate`, including after JSON snapshot restore. Configure the same rates in every worker that resumes the run.
- **Scope.** It applies to every event the SDK sends, product, declared tool and MCP server events alike (MCP servers follow `Configure`, not `Options`, for this).

Sampling runs before the event reaches your analytics-go client, so it works with any client version. A `BeforeEnqueue` hook on the client (where your analytics-go version has one) still sees every event the SDK sends, `sample_rate` included, and stays the place for app-wide drop and edit rules. A hook that drops some of the sampled events itself makes `sample_rate` overstate what arrived: sample in `WithSampleRates`, not in the hook.

## Limits

`WithLimits(Limits{...})` bounds what the SDK holds in memory, whatever the traffic. Reaching a cap is reported as `limit_reached` ([errors](#errors)).

| Field                    | Default  | At the cap                                                                                                                        |
| ------------------------ | -------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `MaxOpenInteractions`    | 10,000   | New interactions are not tracked: your code runs as usual, the open ones keep their data.                                         |
| `InteractionTTL`         | 1 hour   | An interaction open longer (never ended) is sent with `response_status: incomplete` and `end_reason: ttl_expired`, checked as interactions start and end (no timer). |
| `MaxCallsPerInteraction` | 1,000    | Model calls and tool calls past it still count in the totals, `usage_breakdown` and `tool_breakdown`, but are no longer kept one by one: tool outcomes and declared tool events are dropped, even if the outcome came with the call. A tool call past it with an ID and no outcome is still kept, up to as many again, in case a pause waits on it: it is then counted once, in the segment that runs it, so at most twice that many tool calls are kept. |
| `MaxMapEntries`          | 10,000   | Remembered clients, failed attempts held for a retry, each MCP map per server and mcp-go sessions: the oldest entry goes first (a held attempt is recorded at once). Open model-call spans tracked for [span dedup](tracing.md#one-span-per-call-with-the-span-processor): once full, a new one exports without dedup; none is evicted. |

analytics-go's `Enqueue` never refuses an event because its queue is full: when it is, only the sending goroutine waits, so the Go SDK has no full queue to report as `limit_reached`, unlike the Node.js and Python SDKs. Batches analytics-go drops later go to its `Config.Callback` ([delivery](performance.md#delivery)). Masking limits: [privacy](privacy.md#privacy-and-masking).

## Errors

Callback publication and lock guarantees: [Callback re-entry](#callback-re-entry).

Tracking never panics into your code. SDK, integration and configured-hook panics are contained, including panics in the AWS SDK's stream-decoding goroutine. Your call's result or error is returned unchanged. Serializer failures never reach your app. Content serializers follow the [null and omission rules](privacy.md#serializer-behavior); a failed serializer on other extra event properties drops that property.

Background hooks leave the ended interaction ended, reject new calls, retain prior usage and emit one classification or evaluation.

Each contained error has an `ErrorInfo{Kind, Where}`. Kinds:

| Kind                | Meaning                                                                                              |
| ------------------- | ---------------------------------------------------------------------------------------------------- |
| `internal`          | A panic in the SDK or an integration.                                                                |
| `user_callback`     | A hook you configured (redact, identify, event rules, classifier, evaluator, trace ID, `MarshalJSON`) failed or panicked; its documented fallback was used. |
| `credits_unstable` | Pricing changed during all three credits attempts. |
| `pause_refused` | Pause left the interaction unchanged. `reason` (`Reason` in Go) is `in_callback` or `pricing_in_flight`; pricing waits are bounded at 5 s. |
| `limit_reached`     | A memory cap was hit.                                                                                |
| `redaction_timeout` | A masking pattern ran out of time; the field was sent as `[redaction timed out]`.                    |
| `flush_timeout`     | `Flush`, `Shutdown` or a `RunTask` flush, or `CloseClient`, hit its deadline.                                           |
| `flush_failed`      | Your RudderStack client's own Flush or Close failed; the error passed is your client's, unchanged.   |
| `client_outdated`   | The RudderStack client cannot do what the SDK asked of it.                                           |

`WithOnError(fn)` gets the error and its `ErrorInfo`, at most once per kind a minute; a panic in `fn` is swallowed. Without it, each kind is logged once as a warning with the error's type, never its text (it may carry credentials).

## Logging

Warnings (a missing client, a failed `Enqueue`, an invalid status or surface, a dropped `TrackAction` / `TrackScore`, the first classifier failure) go to the default `log/slog` logger, prefixed `[rudderstack-ai]`, mostly once per process. Everything tracking drops or ignores is logged at debug level, errors by type only, never event payloads or captured text. An invalid option value is named by its type only, never the value. Set `RUDDERSTACK_AI_DEBUG=1` (any non-empty value, read on every log call) to write it through the default slog handler whatever its level, or `slog.SetLogLoggerLevel(slog.LevelDebug)` to show it.

## Warehouse column types

RudderStack warehouse destinations create a column with the type of the first value they load and never change it: a whole number such as a free model's `0` cost makes an integer column, and later decimal costs in it are truncated. The SDK never sends a placeholder `0` for an unknown cost.

1. Validate with the tracking plan in [`spec/rudderstack-catalog/`](../spec/rudderstack-catalog), which types costs as decimal numbers.
2. Fix a cost column that is already an integer (`total_cost_usd`, `cost_usd`, `classifier_cost_usd`): PostgreSQL `ALTER COLUMN ... TYPE double precision`, BigQuery `SET DATA TYPE NUMERIC`, ClickHouse `MODIFY COLUMN ... Nullable(Float64)`; Snowflake, Redshift and Databricks need a new `DOUBLE` column, a copy, and a rename. Costs already loaded lost their decimals.

## API reference

| Package                                          | Exports                                                                                                                                                                                                 |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `github.com/rudderlabs/ai-sdk-go` (`rudderstackai`) | `Configure` and the `With*` options, `StartInteraction`, `RunInteraction`, `Interaction`, `RunTask`, `PauseOptions`, `PausedInteraction`, `WithValues`, `Middleware`, `FromContext`, `TrackAction`, `TrackScore`, `TrackClassification`, `Flush`, `FlushTimeout`, `Shutdown`, `CloseClient`, `Flusher`, `ContextFlusher`, `ErrorInfo`, the `Kind*` constants, `Limits`, `RegisterPrice`, `ClearPrices`, `EstimateCost`, `CostOptions`, `ReportedCost`, `ErrorTypeOf`, `ErrorTypeForStatus`, `ClassifierPrompt`, `DefaultOutcomes`, `Ptr`, `Version`, `LibraryName`, `SpecVersion`, the `Integration*`, `Status*`, `Pause*`, `Surface*`, `Action*`, `Evaluator*`, `Classifier*` and `Trigger*` constants; MCP: `WrapMCPServer`, `Elicit`, `TrackMCPEventSubscription`, `WrapMCPEventSender` |
| `.../rudderopenai`                               | `WithTracking`, `Middleware`, `WithProvider`, `WithAnalytics`, `WithStreamUsage`, `ClassifierClient`                                                                                                    |
| `.../rudderanthropic`                            | `WithTracking`, `Middleware`, `WithProvider`, `WithAnalytics`, `ClassifierClient`                                                                                                                       |
| `.../ruddergenai`                                | `HTTPClient`, `Transport`, `WithProvider`, `WithAnalytics`                                                                                                                                              |
| `.../rudderbedrock`                              | `WithTracking`, `WithAnalytics`                                                                                                                                                                         |
| `.../rudderotel`                                 | `TraceID`, `NewSpanProcessor`, `NewExporter`, `RegisterTracing`, `SpanToTrack`, `IsAISpan` ([tracing](tracing.md))                                                                                           |
| `.../ruddermcpgo` (in the root module)           | `WrapServer`, `Options`, `RequestElicitation`, `TrackMCPEventSubscription`, `WrapMCPEventSender`                                                                                                        |

Module dependencies and tested versions: [Compatibility](integrations.md#compatibility).

## Troubleshooting

| Symptom                                    | Cause and fix                                                                                                                         |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| No events, warning `no RudderStack client` | No client anywhere: `Configure(WithAnalytics(client))`.                                                                               |
| Events missing from a job or script        | The process exited before analytics-go sent its batch: `Shutdown(ctx)` then `CloseClient(ctx, client)` on exit, or `Flush(ctx)` while continuing to use the client.                      |
| One answer split into many                 | Calls ran with a context that carries no interaction, or a stream was read after the interaction ended. Pass the interaction's `ctx`. |
| No `total_cost_usd`                        | A model is not priced and no gateway reported a cost, a call failed or was cancelled, or a streamed call had no usage.                 |
| No `ai_interaction_classified`             | No classifier, a call outside an interaction, an answer that failed, or a classifier error (logged once).                             |
| Cost column is an integer in the warehouse | The first loaded value was `0`: see [warehouse column types](configuration.md#warehouse-column-types).                           |

See [Logging](#logging) and [Errors](#errors) for diagnostics.
