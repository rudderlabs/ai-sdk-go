# Integrations

How each integration attaches, what it records, and its edge cases. Event fields: [events.md](events.md). Options: [configuration.md](configuration.md).

- [How integrations work](#how-integrations-work)
- [OpenAI](#openai)
- [Anthropic](#anthropic)
- [Google Gen AI](#google-gen-ai)
- [Amazon Bedrock](#amazon-bedrock)
- [Gateways and OpenAI-compatible endpoints](#gateways-and-openai-compatible-endpoints)
- [Streaming and cancellation](#streaming-and-cancellation)
- [Retries](#retries)
- [Tool output failures](#tool-output-failures)
- [Classifier clients](configuration.md#classifier-clients)
- [Manual](#manual)
- [Go platform limits](#go-platform-limits)

## How integrations work

Each integration is its own Go module and hooks into its provider SDK's own extension point (client middleware, an HTTP transport, or AWS middleware), so the client you construct stays your client.

Every provider integration offers `WithCallPurpose`, `WithFallbackUsed`, and `WithBYOK`. `WithCallDetails` can scope metadata to calls made with a context.

When a model request forces exactly one named tool, matching response calls without a tool result returned before the final End count as structured output, not tool calls: they are excluded from `tool_call_count`, `tool_breakdown` and unknown-outcome declared tool events; model calls and tokens still count. This covers OpenAI Chat and Responses named function choices, Anthropic named tool choices, Bedrock Converse named tool choices, Gemini `ANY` with one allowed function name, and Go provider wrappers used by structured-output frameworks, without library markers.

A tool result or an already emitted tool event keeps the call real. Pause does not exclude candidates: their pending state survives JSON snapshots, and a result after resume keeps them real. Native OpenAI `response_format` / `json_schema` has no tool calls and is unchanged.

Model spans apply the same rule when `gen_ai.request.tool_choice`, `gen_ai.request.toolChoice`, `ai.prompt.toolChoice` or JSON `llm.invocation_parameters` exposes the choice and message attributes expose tool identities; later input tool results or execution spans keep the tool real. Spans without that request choice retain existing accounting. Each exclusion logs the name, call ID and `structured output: forced tool with no result` through `RUDDERSTACK_AI_DEBUG`; the recording-based check command has no exclusion-reason field, so the debug log is the inspection path.

## Interaction ownership

- A call made with a context that carries an interaction (`RunInteraction`, `StartInteraction`, `RunTask`) reports into it. A call without one opens its own one-call interaction and ends it when the call (or its stream) ends: `trigger: system` unless the context carries an identity from `WithValues`, no prompt event, never classified. A call made after its interaction ended logs a warning and becomes such a separate answer, with `parent_interaction_id` set to the ended one.
- **Explicit IDs where context is lost**: `JoinInteraction(ctx, interactionID, JoinOptions{ParentSpanID})` returns a context joined to the interaction open in this process under that ID, for work on a context that lost it (a goroutine started from `context.Background()`, a callback library, an in-process job queue). Wrapper calls, `StartSpan` and `RecordCall` with it report into that interaction, which wins over any interaction the context carries. The context also carries the interaction's own scope in place of the context's `WithValues`: its identity, conversation, account, feature, surface and run labels, its message context and cookie session, its RudderStack client (its own or one a wrapper adopted), and the other values it started with, whether its options or the context supplied them, so `TrackAction`, `TrackScore`, `TrackClassification` and interactions started with it use them, even after the interaction ended. Cookies are not read again. With a reused `InteractionID`, the most recently started open interaction is joined. With `WithEmitSpans(true)`, `ParentSpanID` (a `StartSpan` handle's `SpanID`, even of a span that already ended) parents the SDK spans started directly with that context, in the interaction's trace, ahead of the context's SDK or OpenTelemetry parent. An unknown, paused or ended ID, or a malformed `ParentSpanID`, logs a rate-limited `slog` warning (a usage problem, so not `WithOnError`, which reports SDK failures); the context is then returned unchanged (or without the parent). A nil context is `context.Background()`. It never panics. `ParentSpanID` is ignored with spans off.
- Requests and responses reach the provider SDK unchanged, except that a streamed Chat Completions request asks for usage (see [OpenAI](#openai)). Provider errors are returned to your code unchanged.
- Only model calls are tracked; every other request (embeddings, file uploads, token counting, batches) passes through.

## OpenAI

Integration module: `github.com/rudderlabs/ai-sdk-go/rudderopenai`.

`github.com/openai/openai-go/v3` v3.70.0 or later.

```go
client := openai.NewClient(rudderopenai.WithTracking())

err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID, ConversationID: chatID},
	func(ctx context.Context, in *rudderstackai.Interaction) error {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    "gpt-4.1-mini",
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(question)},
		})
		return err
	})
```

`WithTracking(opts...)` is `option.WithMiddleware(Middleware(opts...))`; pass it to `openai.NewClient` or to a single request. Options: `WithProvider(label)` (default `openai`), `WithAnalytics(client)` and `WithStreamUsage(on)` (below).

- Tracked: POST requests to `.../chat/completions` (Chat Completions) and `.../responses` (Responses API), streamed or not.
- Recorded: `model` (the served model, else the requested one), `requested_model` when another model served it, `prompt_tokens` / `input_tokens` as input (they include cached tokens), cached tokens, `reasoning_tokens`, `service_tier`, the response `id`, the answer text (for the classifier and evaluator only), and `usage.cost` as the call's cost when a gateway reports one.
- `finish_reason`: Chat Completions as sent, `refusal` when the message has a refusal. Responses API: `stop`, `tool_calls` when the output has only tool calls, `refusal` for a refusal part, `length` for `incomplete` at `max_output_tokens`, otherwise the incomplete reason. A `failed` response is a failed call that keeps its usage; a `cancelled` one is a cancelled call.
- Tool calls: `tool_calls` of every returned choice (function and custom tools); Responses output items whose type ends in `_call` (function, custom and hosted tools, `mcp_call` with its server label as `mcp_server`). Results come from `role: "tool"` messages and `*_call_output` items of a later request; they carry no error flag, so `tool_status` stays `unknown` and `tool_breakdown` has no `error_count`, except for [tool output failures](#tool-output-failures).
- Server tools: `web_search_call`, `file_search_call`, `code_interpreter_call` and `image_generation_call` count as server tool calls by kind (`web_search`, ...), for `ServerToolPerCall` fees.
- Streamed Chat Completions: the request gains `stream_options.include_usage` unless you set it, and the extra usage-only chunk is hidden from your code. A host that rejects it (HTTP 400 or 422 naming the parameter, or without an error code) gets the request again as you wrote it; once a host names the parameter, this middleware stops adding it there. `rudderopenai.WithStreamUsage(true)` always adds it, with no resend; `WithStreamUsage(false)` never does. Without usage, a streamed call has no tokens and no cost.
- Not tracked: non-streamed `background: true` Responses requests (only queued; polling is not intercepted).

## Anthropic

Integration module: `github.com/rudderlabs/ai-sdk-go/rudderanthropic`.

`github.com/anthropics/anthropic-sdk-go` v1.78.0 or later.

```go
client := anthropic.NewClient(rudderanthropic.WithTracking())

err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID, ConversationID: chatID},
	func(ctx context.Context, in *rudderstackai.Interaction) error {
		_, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     "claude-haiku-4-5",
			MaxTokens: 1024,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(question))},
		})
		return err
	})
```

`WithTracking(opts...)` is `option.WithMiddleware(Middleware(opts...))`. Options: `WithProvider(label)` (default `anthropic`) and `WithAnalytics(client)`.

- Tracked: Messages API requests (`.../messages`), streamed or not, including beta messages. Token counting and batches pass through.
- Bedrock and Vertex AI clients: pass the tracking option before `bedrock.WithLoadDefaultConfig` / `bedrock.WithConfig` / `vertex.WithGoogleAuth` (middleware runs in registration order, and those options rewrite the request after it), and set the provider label:

```go
client := anthropic.NewClient(
	rudderanthropic.WithTracking(rudderanthropic.WithProvider("aws.bedrock")), // or "gcp.vertex_ai"
	bedrock.WithLoadDefaultConfig(ctx),
)
```

- Tokens: Anthropic reports cache reads and writes apart from `input_tokens`; input is reported as uncached + cache read + cache write, cache reads also as cached input, cache writes as `cache_write_tokens`, 1-hour cache writes (`cache_creation.ephemeral_1h_input_tokens`) as `cache_write_1h_tokens`. `output_tokens_details.thinking_tokens` becomes `reasoning_tokens`.
- `service_tier` from `usage.service_tier` (`priority` is `other`), `fast` when `usage.speed` is `fast`. `usage.server_tool_use` counts (`web_search_requests`, `web_fetch_requests`, ...) are server tool calls by kind (`web_search`, `web_fetch`).
- `finish_reason` is the `stop_reason` as sent (`end_turn`, `tool_use`, `max_tokens`, ...); a last call stopped at `max_tokens` makes the answer `incomplete`.
- Tool calls: `tool_use`, `server_tool_use` and `mcp_tool_use` blocks (`mcp_tool_use` with its `server_name`). Outcomes come from the `tool_result` blocks of a later request in the same interaction (`is_error`): run the whole tool loop inside one interaction.
- An `error` event inside a stream is a failed call (`api_error`) that keeps the tokens seen so far.

## Google Gen AI

Integration module: `github.com/rudderlabs/ai-sdk-go/ruddergenai`.

`google.golang.org/genai` v1.72.0 or later. The genai client has no middleware hook, so tracking sits in its HTTP client.

```go
client, err := genai.NewClient(ctx, &genai.ClientConfig{
	APIKey:     os.Getenv("GEMINI_API_KEY"),
	Backend:    genai.BackendGeminiAPI,
	HTTPClient: ruddergenai.HTTPClient(nil),
})
```

`HTTPClient(base, opts...)` returns a copy of `base` (nil: a zero `http.Client`) whose transport is wrapped; `Transport(base, opts...)` wraps an `http.RoundTripper` (nil: `http.DefaultTransport`) for clients you build yourself. Options: `WithProvider(label)` and `WithAnalytics(client)`.

- Vertex AI with Google Cloud credentials: genai skips its own credential setup when given an `HTTPClient`, so the client must authenticate itself. Obtain an authenticated client with `base, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")` (`golang.org/x/oauth2/google`), check `err`, then wrap it with `ruddergenai.HTTPClient(base)`, or call and check the error from `cfg.UseDefaultCredentials()` on the `ClientConfig` after setting `HTTPClient`. An API key (Gemini API, Vertex AI express mode) needs neither.
- Tracked: `Models.GenerateContent` and `Models.GenerateContentStream`, and chats, which call the same endpoints. Every other method passes through.
- Provider: `gcp.gemini` for the Gemini API, `gcp.vertex_ai` for Vertex AI, detected from each request's URL; `WithProvider` overrides it. The two are [priced separately](configuration.md#pricing).
- Model: `modelVersion` of the response, else the model in the request path (a tuned model's endpoint name).
- Tokens follow `usageMetadata`: input is `promptTokenCount` (which includes `cachedContentTokenCount`) plus `toolUsePromptTokenCount`, output is `candidatesTokenCount` plus `thoughtsTokenCount`, and `thoughtsTokenCount` is also `reasoning_tokens`, so input + output equals `totalTokenCount` and thinking is priced once, at the output rate. In a stream `usageMetadata` is a running total: the last value wins.
- `finish_reason`: `STOP` is `stop`, or `tool_calls` when the response calls a function; `MAX_TOKENS` is `length`; `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` and the image safety reasons are `content_filter`, and so is a blocked prompt (`promptFeedback.blockReason`); other values pass through.
- Tool calls: `functionCall` parts (Vertex AI arguments streamed over several parts with `partialArgs` / `willContinue` are joined into one call), and each `executableCode` part as a `code_execution` call. Outcomes come from `functionResponse` parts of a later request: an `error` key is a failure. The Gemini API sends no function call `id`, so a response can be tied to its call only when the model sent one.
- Errors map by HTTP status, then the Google RPC status (`RESOURCE_EXHAUSTED` `rate_limit`, `DEADLINE_EXCEEDED` `timeout`, `UNAUTHENTICATED` and `PERMISSION_DENIED` `auth_error`, `INVALID_ARGUMENT` `invalid_request`, `CANCELLED` `cancelled`).

## Amazon Bedrock

Integration module: `github.com/rudderlabs/ai-sdk-go/rudderbedrock`.

`github.com/aws/aws-sdk-go-v2/service/bedrockruntime` v1.63.1 or later.

```go
client := bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())

err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID, ConversationID: chatID},
	func(ctx context.Context, in *rudderstackai.Interaction) error {
		_, err := client.Converse(ctx, &bedrockruntime.ConverseInput{
			ModelId:  aws.String("us.anthropic.claude-sonnet-4-5-20250929-v1:0"),
			Messages: []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: question}}}},
		})
		return err
	})
```

`WithTracking(opts...)` returns a `func(*bedrockruntime.Options)`: pass it to `bedrockruntime.New` / `NewFromConfig`, or to a single call. It adds an `APIOptions` middleware (initialize step) that sees the typed input and output, and wraps the client's protocol to observe stream events as the SDK decodes them. Option: `WithAnalytics(client)`. There is no `WithProvider`: the provider is always `aws.bedrock`.

- Tracked: `Converse` and `ConverseStream`. Other operations pass through.
- `model` is the request `ModelId` (Converse responses carry none); `response_id` is the AWS request ID.
- Tokens: AWS `inputTokens` excludes cache reads and writes, so input is `inputTokens + cacheReadInputTokens + cacheWriteInputTokens`; cache reads are also cached input, cache writes `cache_write_tokens`. Streams report usage in the `metadata` event. No reasoning token count.
- `finish_reason` from `stopReason`: `end_turn` and `stop_sequence` are `stop`, `tool_use` is `tool_calls`, `max_tokens` is `length`, `guardrail_intervened` and `content_filtered` are `content_filter`; others as sent.
- Tool calls: `toolUse` blocks; in a stream, `contentBlockStart` tool uses plus their input deltas by content block index. Outcomes: `toolResult` blocks of a later request (`status: error` is a failure); the result value is the single `json` block, the single `text` block (parsed when it is JSON), or the blocks as sent.
- Errors: an exception code containing `Throttling` is `rate_limit`, `AccessDenied` `auth_error`, `ValidationException` `invalid_request`; otherwise the HTTP status. Validation errors raised before sending are recorded as failed calls too.
- A stream that ends without `messageStop` is a failed call (`api_error`).
- Stream read-ahead: the AWS SDK decodes up to two events ahead of what your code has received, so an event counts only once two more were decoded, or when the stream ends or fails (then every decoded event counts). Closing a stream early can leave the last one or two decoded events, such as a tool call your code never saw, uncounted.

## Gateways and OpenAI-compatible endpoints

- Provider labels follow OpenTelemetry `gen_ai.provider.name`: `openai`, `anthropic`, `aws.bedrock`, `gcp.gemini` (Gemini API), `gcp.vertex_ai` (Vertex AI, also for Anthropic on Vertex AI: `rudderanthropic.WithProvider("gcp.vertex_ai")`), `azure.ai.openai`.

Point openai-go at the endpoint and name the provider:

```go
client := openai.NewClient(
	option.WithBaseURL("https://openrouter.ai/api/v1"),
	option.WithAPIKey(os.Getenv("OPENROUTER_API_KEY")),
	rudderopenai.WithTracking(rudderopenai.WithProvider("openrouter")),
)
```

- `openrouter`, `litellm` and `gateway` are routers: a served model with a vendor prefix (`openai/gpt-4o`) is reported under that vendor (`openai`, through the shared alias table in `spec/fixtures/providers-v0.1.json`), with the full name in `model`, and `requested_model` holds the requested model when another one served the call.
- Cost: `usage.cost` (OpenRouter, LiteLLM, in a body or a stream chunk) or the LiteLLM proxy's `x-litellm-response-cost` header is the call's cost, ahead of registered prices. The proxy's `x-litellm-model-name` header is the served model.
- Azure OpenAI: `WithProvider("azure.ai.openai")` plus [`WithAzureDeploymentModels`](configuration.md#pricing) for pricing. Other labels (`vllm`, `groq`, ...) are sent as given.

## Streaming and cancellation

Go detects a streaming response from its `Content-Type` header, so behind a transport or proxy that strips that header the wrapper reads the whole stream before returning it.

Read a stream inside the `RunInteraction` function (or before `End`): when the interaction ends with a stream still open, that call is recorded as cancelled. There is no way to bind a stream to an interaction after the function returned.

```go
err := rudderstackai.RunInteraction(ctx, opts, func(ctx context.Context, in *rudderstackai.Interaction) error {
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	for stream.Next() {
		send(stream.Current())
	}
	return stream.Err()
})
```

| Stream ends                                                        | Recorded as                                                         |
| ------------------------------------------------------------------ | ------------------------------------------------------------------- |
| The provider's terminal event arrives (`[DONE]`, `response.completed`, `message_stop`, a Gemini `finishReason`, Bedrock `messageStop`) | A completed call with its usage; closing the stream afterwards changes nothing |
| You close it early, or the request context is cancelled            | A cancelled call: tokens seen so far kept, never priced             |
| The context deadline passes                                        | A failed call, `timeout`                                            |
| A read error or a provider error event                             | A failed call with the mapped error type and the tokens seen so far |
| It ends without the terminal event (Responses API, Anthropic, Bedrock) | A failed call, `api_error`                                      |
| The interaction ends first                                         | Cancelled, unless the terminal event already arrived                |

Tool calls streamed before a failure still count. A provider that reports usage only at the end (Chat Completions) leaves a cancelled stream with 0 tokens. `time_to_first_token_ms` is measured from the interaction's start to the first streamed text or tool-call delta.

## Retries

HTTP middleware has no hook for the outer SDK method's completion. Retry grouping uses per-invocation request identity, retry headers and response-body probes.

openai-go and anthropic-sdk-go retry inside the client, below the tracking middleware. One SDK method call is one model call, priced once: an attempt the client retries is not recorded, and the final attempt's outcome is. When retries run out, the answer has one failed call. When the context ends while the client waits to retry, the call is recorded as `cancelled` or `timeout`.

With `WithEmitSpans(true)`, Google GenAI retries and connection-error retries also retain one
invocation span. Failed attempts settle after a bounded one-second retry grace even if their
context never ends; a retry arriving later can start another span.

A failed attempt counts as retried when the client sends it again or closes its response unread, which is how both clients behave. With spans disabled, a connection error (no response) outside an interaction is recorded per attempt. A retried failed response that a middleware added before `WithTracking` (outside it) reads is also recorded per attempt.

genai retries below the tracking transport and has no attempt number. The transport groups retries using per-invocation request identity; successful retries record one final call. The AWS SDK retries below the Bedrock tracker, so a Bedrock operation is recorded once, with the final outcome. `ClassifierClient` calls disable retries.

## Tool output failures

The one vendor output convention the integrations read is top-level boolean `successful: false`. A function tool returning a JSON object such as `{"successful": false, "error": "backend down"}`, or a string containing that JSON object, counts as a failed call with `error_type: "api_error"` on its declared tool event and in `tool_breakdown`. This applies to OpenAI, Anthropic, Bedrock and Gemini tool results, and to `rudderotel` product events: tool spans, and the tool responses a model span's input carries, which are read only when the request forces a tool or a structured-output candidate is pending ([How integrations work](#how-integrations-work)).

String `"false"`, arrays, missing keys, true, nested `successful: false` and non-JSON strings retain their existing outcomes. Provider failure signals (`is_error`, `status: error`, a Gemini `error` key, a failed span) keep their existing categories and a call counts once. Returned content, duration and tool identity are preserved.

## Deduplication

The core merges wrapper/span calls with the same provider response ID, or the closest overlapping
call from the other source with matching provider/model, preserving wrapper usage.

A call reported twice with the same provider and `ResponseID` counts once (a repeated report can add missing `AgentName`, `CallPurpose`, credits, `FallbackUsed` and `BYOK`), so an integration and a manual `RecordCall` of the same response do not double count.

A client with the tracking option applied twice tracks each call once.

## Manual

Manual `LLMCall` records accept `CallPurpose`, `FallbackUsed`, `BYOK`, `Credits`, `StartedAt`, `EndedAt`, and `FirstTokenAt`. Observed boundaries produce per-call `latency_ms` and streaming `time_to_first_token_ms`; unknown boundaries stay absent.

For any other model client, report calls yourself:

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
			ResponseID: res.ID, FinishReason: "stop", OutputText: res.Text,
		})
		in.RecordToolCalls(rudderstackai.ToolCall{ID: "call_1", Name: "lookup_order", Args: map[string]any{"order_id": "A1"}})
		in.RecordToolResult("call_1", rudderstackai.ToolResult{Error: rudderstackai.Ptr(false), Duration: rudderstackai.Ptr(120 * time.Millisecond)})
		return nil
	})
```

| `LLMCall` field                                   | Meaning                                                                                                   |
| ------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `Provider`, `Model`                               | OpenTelemetry `gen_ai.provider.name` value (`openai`, `anthropic`, `aws.bedrock`, ...); the served model  |
| `RequestedModel`                                  | The model asked for; sent only when another model served the call                                         |
| `InputTokens`, `OutputTokens`                     | Input includes cached and cache-write tokens; negative values count as 0                                  |
| `ReasoningTokens`                                 | `*int`, a subset of output; nil when the provider reports none                                            |
| `CachedInputTokens`, `CacheWriteTokens`, `CacheWrite1hTokens` | Subsets of input (1-hour writes a subset of cache writes), priced at their own rates           |
| `CostUSD`                                         | `*float64`, a reported cost; wins over registered prices                                                  |
| `ServiceTier`                                     | As the provider reports it; [normalised](events.md#totals-and-usage_breakdown)                           |
| `ServerToolCalls`                                 | Provider-hosted tool calls by kind, `{"web_search": 2}`                                                   |
| `CallCount`, `ErrorCount`                         | An aggregate of several calls (zero means 1) and how many failed (nil: all when `ErrorType` is set)      |
| `AgentName`                                       | Label in `usage_breakdown`                                                                                |
| `ResponseID`                                      | Provider response ID; a repeated one is counted once                                                     |
| `FinishReason`, `ErrorType`                       | A failed call stays unpriced unless `CostUSD` is given                                                    |
| `UsageUnknown`                                    | Tokens unknown or partial: the call stays unpriced                                                        |
| `Integration`                                     | Integration name for `context.wrapperLibrary`; empty is `manual`                                          |
| `OutputText`                                      | Answer text for the classifier and evaluator; sent as `output_text` only with call events and content capture |
| `InputMessages`, `OutputToolCalls`                | GenAI messages sent and tool calls requested; sent only with call events and content capture               |

`ToolCall`: `ID` (merges repeated reports), `Name` (flat `mcp__server__tool` names are split), `MCPServer`, `ParentTool`, `AgentName`, `Error` (`*bool`), `ErrorType`, `Duration` (`*time.Duration`, measured only), `Args` and `Result` (given to declared tool rules, never sent). `ToolResult`: `Error`, `ErrorType`, `Duration`, `Result`. A failure from any report sticks; the first measured duration and the first result win.

An agent that calls MCP servers can join each `mcp_tool_called` to its answer by sending `in.ID()` as `com.rudderstack/interaction_id` in the request `_meta` ([mcp.md](mcp.md#joining-the-calling-agent)); no Go integration adds it for you. The server keeps it only when it is a UUID or 32 hex digits, as `in.ID()` is unless you set `InteractionID` to something else.

## Go platform limits

- Endpoint coverage: [How integrations work](#how-integrations-work) and [OpenAI](#openai).
- Process exit and custom-client flushing: [Flush and shutdown](configuration.md#flush-and-shutdown).

- Bedrock read-ahead: [Amazon Bedrock](#amazon-bedrock).
- Streams and cancellation: [Streaming and cancellation](#streaming-and-cancellation). Pausing: [Pauses and resumes](configuration.md#pauses-and-resumes). Disconnection: [Middleware](configuration.md#context-values).
- Retry limits: [Retries](#retries).

Go resolves one package instance for each import path in a build, including when dependencies use vendoring. Import aliases and repeated wrapping share invocation recognition, so they emit one span. Independently renamed forks cannot recognize each other's wrappers. Lazy Go iterators retain the explicit context passed by the caller, and resolve active interaction ownership and capture policy when provider work starts.

- Mastra is a TypeScript framework, so this SDK has no Mastra integration; use `@rudderstack/ai` (Node.js).
- Provider integrations: OpenAI, Anthropic, Google Gen AI and Bedrock clients. Go has no LangChain,
  LangGraph, OpenAI Agents SDK, Claude Agent SDK or Vercel AI SDK integrations. Use manual spans for framework operations. Enable `WithEmitSpans(true)` and use manual `invoke_agent`, `execute_tool` and
  `retrieval` spans with wrapped model calls to build the same tree. Pass each returned context
  into nested calls, goroutines and streams; wait for workers before ending their parents.
  See the [manual agent/tool/stream example](tracing.md#framework-trees-with-manual-spans).
  The optional `rudderotel` module also provides an OpenTelemetry span processor and exporter.

## Compatibility

Each library lives in its own package, so a go-sdk server never compiles mcp-go; `ruddermcpgo` builds on the root package and so also compiles the go-sdk.

Each module requires the oldest version it supports; Go may upgrade a dependency below that minimum. CI tests the workspace with the newer versions the examples use. Isolated minimum-version tests run on main, release PRs and manual runs, excluding examples.

| Module                                        | Requires and tested at (minimum)                               | Also tested with |
| --------------------------------------------- | -------------------------------------------------------------- | ---------------- |
| All modules                                   | Go 1.26.11                                                      | Go 1.26, 1.27    |
| `github.com/rudderlabs/ai-sdk-go`             | `analytics-go/v4` v4.5.1, MCP `go-sdk` v1.8.0, `mcp-go` v0.55.1 | `mcp-go` v1.1.1, v1.5.0 |
| `.../rudderopenai`                            | `github.com/openai/openai-go/v3` v3.70.0                        | v3.98.0          |
| `.../rudderanthropic`                         | `github.com/anthropics/anthropic-sdk-go` v1.78.0                | v1.84.0          |
| `.../ruddergenai`                             | `google.golang.org/genai` v1.72.0                               | v1.77.0          |
| `.../rudderbedrock`                           | `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` v1.63.1   | v1.63.12          |
| `.../rudderotel`                              | `go.opentelemetry.io/otel` / `trace` / `sdk` v1.47.0           |                  |

Every Go module runs the applicable shared fixtures in [`spec/fixtures/`](../spec/fixtures).

- Go has no native tuple or set type; portable tuple/set fixtures use arrays/slices. Foreign
  runtime exception class fixtures use native Go errors. `MaxTurnsExceededError`,
  `LengthFinishReasonError` and `ContentFilterFinishReasonError` have no existing Go Agent SDK or
  response-parser exception equivalent and are skipped by the classification fixture driver.
- LangChain, OpenAI Agents SDK, Claude Agent SDK and Vercel AI SDK integration fixture cases are explicitly skipped in Go.

The root module holds the interaction API, the `net/http` middleware and MCP support. It depends on analytics-go, the MCP libraries (go-sdk and mcp-go), regexp2 (redaction patterns), google/uuid, and santhosh-tekuri/jsonschema (used only by the SDK's own schema tests, so it is not compiled into your binary). Each provider integration, and the OpenTelemetry helper `rudderotel`, is its own module, so an MCP-only service does not pull provider SDKs.

## Examples

| Example                                               | Shows                                                                                          |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| [`openai`](../examples/openai)                           | `rudderopenai`: a streamed Chat Completions answer, a registered price, `TrackAction` feedback |
| [`anthropic`](../examples/anthropic)                     | `rudderanthropic`: a streamed Messages answer with cache reads and writes                      |
| [`google-genai`](../examples/google-genai)               | `ruddergenai`: a tool call, then a streamed answer, with `tool_breakdown`                      |
| [`bedrock-converse`](../examples/bedrock-converse)       | `rudderbedrock`: `Converse` with a tool, then `ConverseStream`, priced through an inference profile |
| [`background-agent`](../examples/background-agent)       | `RunTask`: system-triggered work with a chained sub-task and `TrackScore`                      |
| [`approval-loop`](../examples/approval-loop)             | `RunTask` that pauses twice for approval and resumes from a stored snapshot; `ai_task_started` |
| [`manual`](../examples/manual)                           | `StartInteraction`, `RecordCall`, `RecordToolCalls`, a declared tool event, a custom classifier |
| [`mcp-server`](../examples/mcp-server)                   | stdio and streamable HTTP, `request_context`, a declared event, `submit_feedback`           |
| [`mcp-server-mcpgo`](../examples/mcp-server-mcpgo)       | The same server on mcp-go with `ruddermcpgo.WrapServer`, 2025-06-18 and 2026-07-28             |
| [`mcp-stateless-http`](../examples/mcp-stateless-http)   | Stateless streamable HTTP on 2026-07-28, confirmation through an `input_required` elicitation  |
| [`mcp-auth`](../examples/mcp-auth)                       | A bearer token check and `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |

From the repository root: `go run ./examples/<name> --check` runs one end to end against a local mock data plane and a mock LLM (no credentials); `--live` sends the same events to `RUDDERSTACK_DATA_PLANE_URL` with `RUDDERSTACK_WRITE_KEY`; without flags a provider example makes one real call and an MCP example serves. Details: [examples/README.md](../examples/README.md).
