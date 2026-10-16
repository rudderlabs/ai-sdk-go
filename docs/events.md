# Events

Every AI interaction event this SDK sends, with sample payloads and how each property is derived. MCP server events (`mcp_*`) are in [mcp.md](mcp.md). The JSON Schema is [`spec/ai-events-v0.3.schema.json`](../spec/ai-events-v0.3.schema.json), shared with the Node.js and Python SDKs; a RudderStack data catalog and tracking plan are in [`spec/rudderstack-catalog/`](../spec/rudderstack-catalog).

- [Common fields](#common-fields)
- [ai_user_prompt_created](#ai_user_prompt_created)
- [ai_response_received](#ai_response_received)
- [Status](#status)
- [Totals and usage_breakdown](#totals-and-usage_breakdown)
- [Cost](#cost)
- [tool_breakdown](#tool_breakdown)
- [ai_llm_response_received](#ai_llm_response_received)
- [Declared tool events](#declared-tool-events)
- [ai_interaction_classified](#ai_interaction_classified)
- [ai_interaction_scored](#ai_interaction_scored)
- [ai_user_action](#ai_user_action)
- [Error types](#error-types)
- [context.wrapperLibrary](#contextwrapperlibrary)

## Common fields

Every event is an analytics-go `Track` with `userId` or `anonymousId`; an event with neither is sent with `anonymousId: "ai_system"`. Identity, `context.sessionId` and the RudderStack client are resolved as described in [configuration.md](configuration.md#identity-and-cookies).

Properties on every interaction event (prompt, answer, per-call, declared tool, classified, scored):

| Property                | Source                                                                                              |
| ----------------------- | --------------------------------------------------------------------------------------------------- |
| `conversation_id`       | `InteractionOptions.ConversationID`, else the context's, else a minted UUID v4                      |
| `interaction_id`        | `InteractionOptions.InteractionID`, else a UUID v4                                                  |
| `feature`               | `Feature` of the options, else the context's                                                        |
| `surface`               | `Surface` of the options, else the context's; a value outside the `Surface` constants is dropped with a warning |
| `account_id`            | `AccountID` of the options, else the context's                                                      |
| `on_behalf_of`          | `OnBehalfOf` of the options, else the context's                                                     |
| `trigger_source`        | `TriggerSource`; a [task](configuration.md#taskoptions) with a parent defaults to `agent`           |
| `task_name`             | The `RunTask` name, or `InteractionOptions.TaskName`                                                |
| `trigger_event_id`      | `TriggerEventID`, for example the `event_id` of an MCP Events delivery                             |
| `parent_interaction_id` | `ParentInteractionID`, the enclosing interaction of a chained task, or the ended interaction a late call belonged to |

Empty values are left out. The SDK never sends `source` (its events are the server copies) or `spec_version` (the version travels in [`context.wrapperLibrary`](#contextwrapperlibrary)). `ai_user_action` carries only `conversation_id`, `interaction_id`, `account_id`, `on_behalf_of` and `surface` of these.

## ai_user_prompt_created

The user asked something. Sent for an interaction with `Trigger: TriggerUser` (the default) unless `NoPromptEvent` is set; never for tasks or for interactions an integration opens on its own.

```json
{
  "event": "ai_user_prompt_created",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "feature": "support_bot",
    "surface": "web",
    "input_method": "text",
    "attachment_count": 1,
    "attachment_types": ["image"],
    "intent": "refund_request"
  }
}
```

- The event is held until the interaction's first model call starts (or a call is recorded, or the interaction ends), so it carries that call's integration in `context.wrapperLibrary` and the integration's client. Its timestamp is the moment `StartInteraction` ran.
- `intent` is the one set by `InteractionOptions.Intent` or `SetIntent` before the event goes out; a later `SetIntent` lands on `ai_response_received` only.
- `input_method`, `attachment_count` and `attachment_types` come from `InputMethod`, `AttachmentCount` and `AttachmentTypes`. Attachment kinds are `image`, `audio`, `video`, `pdf` or `file` (any other value becomes `file`), lower-cased and each sent once. Never file names or content.
- `prompt_text`: `PromptText`, only with content capture on, [masked](configuration.md#privacy-and-masking).

## ai_response_received

One per interaction, sent by `Interaction.End` (or when `RunInteraction` / `RunTask` returns), with totals across every call.

```json
{
  "event": "ai_response_received",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "feature": "support_bot",
    "response_status": "success",
    "trigger": "user",
    "intent": "refund_request",
    "outcome": "resolved",
    "primary_model": "gpt-4.1-mini-2025-04-14",
    "primary_provider": "openai",
    "latency_ms": 2140,
    "time_to_first_token_ms": 610,
    "llm_call_count": 2,
    "tool_call_count": 1,
    "total_input_tokens": 2310,
    "total_output_tokens": 164,
    "total_tokens": 2474,
    "total_cached_input_tokens": 1024,
    "total_cache_write_tokens": 0,
    "total_cost_usd": 0.0008792,
    "finish_reason": "stop",
    "usage_breakdown": [
      {
        "provider": "openai",
        "model": "gpt-4.1-mini-2025-04-14",
        "call_count": 2,
        "input_tokens": 2310,
        "output_tokens": 164,
        "total_tokens": 2474,
        "cached_input_tokens": 1024,
        "cache_write_tokens": 0,
        "service_tier": "standard",
        "error_count": 0,
        "cost_usd": 0.0008792
      }
    ],
    "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"
  }
}
```

| Property                 | Derivation                                                                                                   |
| ------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `response_status`        | See [Status](#status)                                                                                        |
| `pause_reason`           | Only with status `paused`: `EndOptions.PauseReason`, default `other`                                         |
| `trigger`                | `user` or `system`                                                                                           |
| `client_disconnected`    | `true` only when `EndOptions.ClientDisconnected` was set                                                     |
| `primary_model`, `primary_provider` | The last successful call's, else the last call's                                                  |
| `latency_ms`             | From `StartInteraction` to `End`, rounded                                                                    |
| `time_to_first_token_ms` | From `StartInteraction` to the first streamed token any integration saw; absent without a stream            |
| `llm_call_count`         | Sum of `CallCount` (1 per call) over recorded calls, failed ones included                                    |
| `tool_call_count`        | Tool calls the model made, de-duplicated by tool call ID, including provider-hosted tools                   |
| `total_*_tokens`, `total_cost_usd` | See [Totals](#totals-and-usage_breakdown) and [Cost](#cost)                                         |
| `finish_reason`          | The last successful call's                                                                                   |
| `error_type`             | Only when the status is not `success`; see [Error types](#error-types)                                      |
| `intent`, `outcome`      | `InteractionOptions.Intent` / `SetIntent`, `SetOutcome`                                                      |
| `usage_breakdown`        | One entry per agent, provider, model, requested model and service tier                                       |
| `tool_breakdown`         | Only with tool breakdown on; see [tool_breakdown](#tool_breakdown)                                           |
| `response_text`          | `EndOptions.ResponseText`, only with content capture on, masked                                              |
| `trace_id`               | `TraceID` / `SetTraceID`, else the function registered with [`WithTraceIDFrom`](configuration.md#trace_id) (such as `rudderotel.TraceID`); absent otherwise |

`EndOptions.ExtraProperties` adds your own properties; names the SDK owns (every property above, plus `spec_version`, `source`, `prompt_text`, `input_method`, `attachment_count`, `attachment_types`) are ignored.

A background task's answer adds its system-actor fields:

```json
{
  "event": "ai_response_received",
  "anonymousId": "ai_system",
  "properties": {
    "trigger": "system",
    "account_id": "workspace_acme",
    "on_behalf_of": "user_42",
    "trigger_source": "agent",
    "task_name": "draft-status-update",
    "parent_interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b"
  }
}
```

`trigger_source` is free text; documented values are `schedule`, `webhook`, `slack`, `queue`, `agent`, `api`, `user` and `mcp_event`.

## Status

`response_status` is `success`, `error`, `timeout`, `cancelled`, `paused` or `incomplete`, decided in this order:

1. `EndOptions.Status`, when it is one of the `Status*` constants (any other value logs a warning and is ignored). `RunInteraction` and `RunTask` pass the status of the error `fn` returned (`ErrorTypeOf(err)`: `timeout` for `timeout`, `cancelled` for `cancelled`, else `error`), and `error` with `api_error` when `fn` panics.
2. Otherwise the last recorded call decides: a failed call gives the status of its `ErrorType` (`timeout`, `cancelled`, `max_turns` / `max_budget_usd` / `length` → `incomplete`, anything else `error`), even after earlier successful calls and even when your code handled the error. A failed call followed by a successful retry is a success.
3. A successful last call whose `FinishReason` is `length` or `max_tokens` makes the answer `incomplete` with `error_type: length`; `content_filter` makes it `error` with `error_type: content_filter`. A refusal (`finish_reason: refusal`) stays `success`.
4. Otherwise `success`, also for an interaction without calls.

`cancelled` always carries `error_type: cancelled` unless another one was given. `paused` and `incomplete` with a cap such as `max_turns` are set only through `EndOptions`: no Go integration produces them on its own.

## Totals and usage_breakdown

Token counts follow one convention for every provider: input includes cached and cache-write tokens, cache reads are also `cached_input_tokens`, and reasoning tokens are part of output. Each integration [normalises](integrations.md) its provider's usage to this.

- `total_input_tokens`, `total_output_tokens`, `total_cached_input_tokens`, `total_cache_write_tokens`: sums over every recorded call. `total_tokens` = input + output.
- `total_reasoning_tokens`: the sum over calls that reported reasoning tokens (a reported `0` counts); absent when none did.
- A `cancelled` answer with no recorded call has no token totals: its usage is unknown, not zero.

`usage_breakdown` groups calls by agent name, provider, model, requested model and service tier, in the order first seen:

| Entry field              | Meaning                                                                                                   |
| ------------------------ | --------------------------------------------------------------------------------------------------------- |
| `agent_name`             | `LLMCall.AgentName`, when set                                                                             |
| `provider`, `model`      | As recorded (`null` when empty)                                                                           |
| `requested_model`        | The model the app asked for, only when another model served the call (not a dated version of the same one) |
| `call_count`             | Calls in the group                                                                                        |
| `input_tokens`, `output_tokens`, `total_tokens`, `cached_input_tokens`, `cache_write_tokens` | Sums                                   |
| `reasoning_tokens`       | Sum, when any call in the group reported one                                                             |
| `cache_write_1h_tokens`  | Anthropic 1-hour cache writes (part of `cache_write_tokens`), when there were any                         |
| `service_tier`           | `batch`, `flex`, `standard`, `fast` or `other`: provider `default`, `auto` and `standard_only` are `standard`; `priority` is `fast`, except at Anthropic (Priority Tier) where it is `other`; Anthropic fast mode (`speed: fast`) is `fast`; any other value is `other` |
| `server_tool_call_count` | Provider-hosted tool calls billed per call, when there were any                                           |
| `server_tool_cost_usd`   | Their fees, sent only when every call in the group that used one was priced                               |
| `error_count`            | Failed calls, retries included                                                                            |
| `cost_usd`               | Sum of the group's call costs, under the same rule as `total_cost_usd`                                    |

A call reported twice with the same provider and `ResponseID` counts once (the second report only adds a missing `AgentName`), so an integration and a manual `RecordCall` of the same response do not double count.

Gateways (provider `openrouter`, `litellm` or `gateway`) report a served model with a vendor prefix (`openai/gpt-4o`): the call is recorded under that vendor (`openai`, through the shared alias table in `spec/fixtures/providers-v0.1.json`), and `model` keeps the full served name.

## Cost

No prices ship with the SDK. A call's cost is, in order:

1. `LLMCall.CostUSD`, a cost the provider or gateway reported (OpenRouter and LiteLLM `usage.cost`, the LiteLLM proxy's `x-litellm-response-cost` header);
2. otherwise the [registered price](configuration.md#pricing) of the call's provider and model, with its tier multiplier, long-context rates and server-tool fees;
3. otherwise none: the call keeps its tokens and has no `cost_usd`.

A failed call (`ErrorType` set) or one with `UsageUnknown` is priced only from a reported `CostUSD`; a cancelled call is never priced. `total_cost_usd` is the sum of the priced calls, sent only when at least one call was priced and every successful call was, so a partial total never understates spend. A real priced `0` is sent as `0`. Costs are rounded half-up to 8 decimals.

## tool_breakdown

With `WithToolBreakdown(true)` (or `ToolBreakdown` on one interaction or task), `ai_response_received` adds one entry per agent, MCP server, tool and parent tool:

```json
"tool_breakdown": [
  { "agent_name": "planner", "mcp_server": "github", "tool_name": "search", "parent_tool": "task", "call_count": 2, "error_count": 1, "error_types": { "denied": 1 }, "duration_ms": 840, "measured_count": 1, "max_duration_ms": 840 },
  { "tool_name": "lookup_order", "call_count": 3 }
]
```

- Built from the same de-duplicated calls as `tool_call_count`, so `call_count`s add up to it. A call without a name (`RecordToolCallCount`) is `unknown`. A flat `mcp__server__tool` name becomes `mcp_server: "server"`, `tool_name: "tool"`; an explicit `ToolCall.MCPServer` wins. Entries are sorted by `agent_name`, `mcp_server`, `tool_name`, `parent_tool` in UTF-16 order, as the Node.js and Python SDKs sort.
- `error_count` is present when the outcome of at least one call in the entry is known. `error_types` counts failures by `rate_limit`, `timeout`, `cancelled`, `denied`, `auth_error`, `invalid_request`, `connection_error` or `api_error` (any other `ErrorType` is `api_error`). A failure from any source sticks.
- `duration_ms` sums the measured durations, `measured_count` says how many calls were measured, `max_duration_ms` is the largest. Only `ToolCall.Duration` / `ToolResult.Duration` you report are measured; no Go integration measures tool execution.
- Names and counts only: tool arguments and results are never sent.

What each integration observes:

| Integration     | Tool calls from                                                                         | Outcome (`error_count`) from                                                  |
| --------------- | --------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `rudderopenai`  | Chat Completions `tool_calls` of every choice; Responses output items named `*_call`   | Not observed: tool results carry no error flag                                |
| `rudderanthropic` | `tool_use`, `server_tool_use` and `mcp_tool_use` blocks                               | `tool_result` blocks of a later request in the same interaction (`is_error`) |
| `ruddergenai`   | `functionCall` parts; each `executableCode` part is a `code_execution` call             | `functionResponse` parts of a later request: an `error` key is a failure      |
| `rudderbedrock` | `toolUse` blocks (streamed: by content block index)                                     | `toolResult` blocks of a later request (`status: error` is a failure)         |
| Manual          | `RecordToolCalls`, `RecordToolCallCount`                                                | `ToolCall.Error`, `RecordToolResult`                                          |

## ai_llm_response_received

Only with `WithLLMCallEvents(true)`: one per recorded call, sent as it is recorded. Totals belong on `ai_response_received`; do not add these to it.

```json
{
  "event": "ai_llm_response_received",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "provider": "anthropic",
    "model": "claude-haiku-4-5-20251001",
    "input_tokens": 1200,
    "output_tokens": 80,
    "total_tokens": 1280,
    "cached_input_tokens": 1000,
    "cache_write_tokens": 0,
    "service_tier": "standard",
    "cost_usd": 0.00052,
    "response_id": "msg_01XFDUDYJgAACzvnptvVoYEL",
    "finish_reason": "end_turn"
  }
}
```

Also, when known: `agent_name`, `requested_model`, `reasoning_tokens`, `cache_write_1h_tokens`, `server_tool_call_count`, `server_tool_cost_usd`, `error_type`. `provider` and `model` are `null` when unknown. `finish_reason` is as the integration reports it (Anthropic's `end_turn` and `max_tokens` are kept as sent; the others are normalised, see [integrations.md](integrations.md)).

## Declared tool events

Turn tool calls into your own product events:

```go
err := rudderstackai.Configure(rudderstackai.WithEventRules(rudderstackai.EventRules{
	Tools: map[string]rudderstackai.ToolEventRule{
		"save_memory": {Event: "memory_captured"},
		"create_ticket": {
			Event:      "ticket_created",
			Properties: func(args map[string]any, result any) map[string]any { return map[string]any{"priority": args["priority"]} },
			When:       func(args map[string]any, result any) bool { return result != nil },
		},
	},
}))
```

Each call of `create_ticket` then sends one `ticket_created`:

```json
{
  "event": "ticket_created",
  "userId": "user_42",
  "properties": {
    "priority": "high",
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "trigger": "user",
    "tool_name": "create_ticket",
    "tool_call_id": "toolu_01A2b3C4",
    "tool_status": "success"
  }
}
```

- When: once per de-duplicated tool call, as soon as its outcome (`Error`) or output (`Result`) is known; otherwise when the interaction ends, with `tool_status: "unknown"`. Calls of tools without a rule send nothing.
- SDK-owned properties: `tool_name`, `tool_call_id` (absent without a provider ID), `tool_status` (`success`, `error` or `unknown`), `tool_duration_ms` (only when a duration was reported), `agent_name` when known, `trigger` and the [common fields](#common-fields). They win over what `Properties` returns.
- `args` is the call's arguments when they are a JSON object (JSON text is parsed), else nil; `result` is what the integration saw of the tool's output (a later request's tool message or result block, parsed when it is JSON), or `ToolResult.Result`. Neither is sent unless `Properties` returns it.
- `When` returning false, or panicking, skips the event. If `Properties` panics, the event is sent without your properties.
- Rule keys are tool names as the model calls them (`mcp__server__tool` for flat MCP names). Event names starting with `ai_` or `mcp_` are rejected by `Configure`. `WithEventRules` replaces earlier rules. These rules are separate from the `Options.EventRules` of [WrapMCPServer](mcp.md#declared-tool-events), which use the same types.
- Outside an interaction each call is its own answer, so a tool's result, sent in the next request, never reaches the rule: the event goes out with `tool_status: "unknown"` and a nil `result`. Run the whole tool loop inside one `RunInteraction` when a rule needs results.

## ai_interaction_classified

Opt-in with [`WithClassifier`](configuration.md#classifier-and-evaluator). After each successful answer of an interaction you opened, your classifier labels it; nothing waits for it.

```json
{
  "event": "ai_interaction_classified",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "trigger": "user",
    "intent": "refund_request",
    "outcome": "resolved",
    "classifier": "model",
    "classifier_model": "claude-haiku-4-5-20251001",
    "classifier_duration_ms": 640,
    "classifier_cost_usd": 0.00041
  }
}
```

- `intent` is one of `ClassifierOptions.Intents`, else `other` (with your own `Classify` and no `Intents`, any non-empty value is kept). `outcome` is one of `Outcomes` (default `DefaultOutcomes`: `resolved`, `partial`, `unresolved`, `escalated`, `unclear`), else `unclear`.
- `classifier` is `model` for a `ClassifierClient`, `custom` for your `Classify` function. `classifier_model` is the model the reply named, else the configured one. `classifier_duration_ms` times the classification.
- `classifier_cost_usd`: the classifier call priced with your registered prices, when its usage is known and its model priced. Never added to the interaction's totals; the classifier's own call is never tracked.
- `intent_summary`: with `IntentSummary` (model client) or from your `Classify` result. One sentence, at most 25 words and 280 characters, masked, and withheld when it still matches a sensitive pattern.
- Which interactions: one classification per successful interaction you open (`StartInteraction`, `RunInteraction`) with `Classify` on (the default for user-triggered ones). Tasks are off unless `TaskOptions.Classify` is set; interactions an integration opens on its own are never classified. Errors, cancellations and other non-success statuses are skipped.
- Input: the prompt (`PromptText`, else the last user message of the request messages an integration kept), the answer (`EndOptions.ResponseText`, else the last successful call's answer text), the names of the tools called (never arguments or results) and earlier messages, within a 12,000-character budget that keeps the start and end of long text. The prompt fences the turn as untrusted data and asks for JSON from closed lists ([`ClassifierPrompt`](configuration.md#classifier-and-evaluator)). An interaction with neither prompt nor answer text is skipped with a warning.
- Failures: a timeout (default 5 seconds), an error, a panic or an unparseable reply sends nothing and logs one warning per process. Up to 16 classifications and evaluations run at once and up to 1,000 wait; beyond that, work is dropped with a warning at most once a minute.

## ai_interaction_scored

One event per score: from your evaluator after each successful answer, or from `TrackScore` for a score from elsewhere (a human review, an offline judge).

```json
{
  "event": "ai_interaction_scored",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "trigger": "user",
    "evaluation_name": "groundedness",
    "score_value": 0.82,
    "score_label": "pass",
    "evaluator": "model",
    "sampled": true
  }
}
```

- `evaluation_name`, `evaluator` (`EvaluatorModel`, `EvaluatorCode` or `EvaluatorHuman`) and at least one of a finite `score_value` and a non-empty `score_label` are required; a score without them is dropped with a warning. `sampled` is sent when you set it.
- `explanation`: `Score.Explanation`, only with content capture on, masked, trimmed and capped at 280 characters.
- Evaluator: runs for every interaction that ended `success`, including tasks and interactions an integration opened, after `ai_response_received`, with a 5-second timeout and an untracked context. An error, panic or timeout sends nothing.
- `TrackScore` needs `InteractionID` and `ConversationID` (the latter falls back to the context); it adds `feature`, `surface`, `account_id`, `on_behalf_of` and `trigger` when given, and is tagged `integration: "manual"`.

## ai_user_action

What the user did with an answer, sent by `TrackAction`, typically from a feedback endpoint.

```json
{
  "event": "ai_user_action",
  "userId": "user_42",
  "properties": {
    "conversation_id": "8f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f",
    "interaction_id": "3b9e5d0a-7c2f-4e18-9a6b-2c4d6e8f0a1b",
    "action_type": "feedback_given",
    "action_details": { "feedback_type": "thumbs", "feedback_value": 1 }
  }
}
```

- `action_type`: `feedback_given`, `feedback_retracted`, `copied_response`, `shared`, `reported`, `regenerated`, `accepted`, `rejected`, `edited` or `clicked_through` (the `Action*` constants). Another value is sent with a warning.
- `action_details`: your map, with `feedback_type`, `feedback_text`, `card_kind` and `target_type` dropped (with a warning) when not strings; `card_kind` and `target_type` also when empty. `feedback_text` is sent only with content capture on, masked.
- Send `feedback_retracted` with the same `feedback_type` when a user takes feedback back: the latest event per `interaction_id` is the final state.
- `InteractionID` and `ConversationID` (falling back to the context) are required; without them the event is dropped with a warning. `WithServerFeedback(false)` turns `TrackAction` off when the browser sends its own copy (`source: "client"`).

## Error types

`error_type` on `ai_response_received` and `ai_llm_response_received` is one of:

| Value                                 | From                                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `rate_limit`                          | HTTP 429, OpenAI `insufficient_quota` / `rate_limit_exceeded` codes, AWS throttling, Google `RESOURCE_EXHAUSTED` |
| `timeout`                             | HTTP 408 and 504, `context.DeadlineExceeded`, a network timeout, Google `DEADLINE_EXCEEDED`                  |
| `cancelled`                           | `context.Canceled`, a stream closed before it finished, Google `CANCELLED`                                   |
| `auth_error`                          | HTTP 401 and 403, AWS `AccessDeniedException`, Google `UNAUTHENTICATED` / `PERMISSION_DENIED`                 |
| `invalid_request`                     | HTTP 400 and 422, AWS `ValidationException`, Google `INVALID_ARGUMENT`                                        |
| `connection_error`                    | A network error without a response                                                                            |
| `api_error`                           | Anything else, including provider 5xx and error events inside a stream                                       |
| `length`, `content_filter`            | Derived from the last call's `finish_reason` (see [Status](#status))                                         |
| `max_turns`, `max_budget_usd`, others | Only when you pass them in `EndOptions.ErrorType` or `LLMCall.ErrorType`                                      |

`rudderstackai.ErrorTypeOf(err)` applies the same mapping to any error: the integrations register their SDK's error types (`*openai.Error`, `*anthropic.Error`, `genai.APIError`, AWS `smithy.APIError`), then errors with `ErrorCode()` or `HTTPStatusCode()`, then context and network errors. Wrapped errors are unwrapped, so a retry library's error maps as the provider error behind it. `ErrorTypeForStatus(status)` maps an HTTP status alone.

## context.wrapperLibrary

Every event carries `context.wrapperLibrary` beside the `context.library` analytics-go sets:

```json
{
  "wrapperLibrary": {
    "name": "ai-sdk-go",
    "version": "0.1.0",
    "specVersion": "0.3.0",
    "integration": "openai"
  }
}
```

`integration` is the integration that started or recorded the interaction's first model call (`openai`, `anthropic`, `google-genai`, `bedrock`), `manual` for `LLMCall.Integration` left empty, `TrackAction`, `TrackScore` and interactions without calls, or `mcp` for [MCP server events](mcp.md). If your own message context (`Context` of the options or `WithValues`) has a `wrapperLibrary`, it is nested inside the SDK's as `wrapperLibrary.wrapperLibrary`. Other keys of your context are sent as they are.
