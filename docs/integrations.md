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
- [Classifier clients](#classifier-clients)
- [Manual](#manual)
- [Limits and differences from Node.js](#limits-and-differences-from-nodejs)

## How integrations work

Each integration is its own Go module and hooks into its provider SDK's own extension point (client middleware, an HTTP transport, or AWS middleware), so the client you construct stays your client:

| Integration     | Module (`go get`)                                    | Attach with                                                       | Minimum Go |
| --------------- | ---------------------------------------------------- | ----------------------------------------------------------------- | ---------- |
| OpenAI          | `github.com/rudderlabs/ai-sdk-go/rudderopenai`       | `openai.NewClient(rudderopenai.WithTracking())`                   | 1.25       |
| Anthropic       | `github.com/rudderlabs/ai-sdk-go/rudderanthropic`    | `anthropic.NewClient(rudderanthropic.WithTracking())`             | 1.24       |
| Google Gen AI   | `github.com/rudderlabs/ai-sdk-go/ruddergenai`        | `genai.ClientConfig{HTTPClient: ruddergenai.HTTPClient(nil)}`     | 1.24       |
| Amazon Bedrock  | `github.com/rudderlabs/ai-sdk-go/rudderbedrock`      | `bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())` | 1.24       |

Each module's minimum Go version is its provider SDK's.

- A call made with a context that carries an interaction (`RunInteraction`, `StartInteraction`, `RunTask`) reports into it. A call without one opens its own one-call interaction and ends it when the call (or its stream) ends: `trigger: system` unless the context carries an identity from `WithValues`, no prompt event, never classified. A call made after its interaction ended logs a warning and becomes such a separate answer, with `parent_interaction_id` set to the ended one.
- Requests and responses reach the provider SDK unchanged, except that a streamed Chat Completions request asks for usage (see [OpenAI](#openai)). Provider errors are returned to your code unchanged.
- A client with the tracking option applied twice tracks each call once.
- Only model calls are tracked; every other request (embeddings, file uploads, token counting, batches) passes through.

## OpenAI

`github.com/openai/openai-go/v3`, tested with v3.70.0.

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

`WithTracking(opts...)` is `option.WithMiddleware(Middleware(opts...))`; pass it to `openai.NewClient` or to a single request. Options: `WithProvider(label)` (default `openai`) and `WithAnalytics(client)`.

- Tracked: POST requests to `.../chat/completions` (Chat Completions) and `.../responses` (Responses API), streamed or not.
- Recorded: `model` (the served model, else the requested one), `requested_model` when another model served it, `prompt_tokens` / `input_tokens` as input (they include cached tokens), cached tokens, `reasoning_tokens`, `service_tier`, the response `id`, the answer text (for the classifier and evaluator only), and `usage.cost` as the call's cost when a gateway reports one.
- `finish_reason`: Chat Completions as sent, `refusal` when the message has a refusal. Responses API: `stop`, `tool_calls` when the output has only tool calls, `refusal` for a refusal part, `length` for `incomplete` at `max_output_tokens`, otherwise the incomplete reason. A `failed` response is a failed call that keeps its usage; a `cancelled` one is a cancelled call.
- Tool calls: `tool_calls` of every returned choice (function and custom tools); Responses output items whose type ends in `_call` (function, custom and hosted tools, `mcp_call` with its server label as `mcp_server`). Results come from `role: "tool"` messages and `*_call_output` items of a later request; they carry no error flag, so `tool_status` stays `unknown` and `tool_breakdown` has no `error_count`.
- Server tools: `web_search_call`, `file_search_call`, `code_interpreter_call` and `image_generation_call` count as server tool calls by kind (`web_search`, ...), for `ServerToolPerCall` fees.
- Streamed Chat Completions: the request gains `stream_options.include_usage` unless you set it, and the extra usage-only chunk is hidden from your code. A host that rejects it (HTTP 400 or 422 naming the parameter, or without an error code) gets the request again as you wrote it; once a host names the parameter, this middleware stops adding it there. Without usage, a streamed call has no tokens and no cost.
- Not tracked: non-streamed `background: true` Responses requests (only queued; polling is not intercepted).

## Anthropic

`github.com/anthropics/anthropic-sdk-go`, tested with v1.78.0.

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

`google.golang.org/genai`, tested with v1.72.0. The genai client has no middleware hook, so tracking sits in its HTTP client.

```go
client, err := genai.NewClient(ctx, &genai.ClientConfig{
	APIKey:     os.Getenv("GEMINI_API_KEY"),
	Backend:    genai.BackendGeminiAPI,
	HTTPClient: ruddergenai.HTTPClient(nil),
})
```

`HTTPClient(base, opts...)` returns a copy of `base` (nil: a zero `http.Client`) whose transport is wrapped; `Transport(base, opts...)` wraps an `http.RoundTripper` (nil: `http.DefaultTransport`) for clients you build yourself. Options: `WithProvider(label)` and `WithAnalytics(client)`.

- Vertex AI with Google Cloud credentials: genai skips its own credential setup when given an `HTTPClient`, so the client must authenticate itself. Wrap an authenticated client, `ruddergenai.HTTPClient(google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform"))` (`golang.org/x/oauth2/google`), or call `cfg.UseDefaultCredentials()` on the `ClientConfig` after setting `HTTPClient`. An API key (Gemini API, Vertex AI express mode) needs neither.
- Tracked: `Models.GenerateContent` and `Models.GenerateContentStream`, and chats, which call the same endpoints. Every other method passes through.
- Provider: `google` for the Gemini API, `google.vertex` for Vertex AI, detected from each request's URL; `WithProvider` overrides it. The two are [priced separately](configuration.md#pricing).
- Model: `modelVersion` of the response, else the model in the request path (a tuned model's endpoint name).
- Tokens follow `usageMetadata`: input is `promptTokenCount` (which includes `cachedContentTokenCount`) plus `toolUsePromptTokenCount`, output is `candidatesTokenCount` plus `thoughtsTokenCount`, and `thoughtsTokenCount` is also `reasoning_tokens`, so input + output equals `totalTokenCount` and thinking is priced once, at the output rate. In a stream `usageMetadata` is a running total: the last value wins.
- `finish_reason`: `STOP` is `stop`, or `tool_calls` when the response calls a function; `MAX_TOKENS` is `length`; `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` and the image safety reasons are `content_filter`, and so is a blocked prompt (`promptFeedback.blockReason`); other values pass through.
- Tool calls: `functionCall` parts (Vertex AI arguments streamed over several parts with `partialArgs` / `willContinue` are joined into one call), and each `executableCode` part as a `code_execution` call. Outcomes come from `functionResponse` parts of a later request: an `error` key is a failure. The Gemini API sends no function call `id`, so a response can be tied to its call only when the model sent one.
- Errors map by HTTP status, then the Google RPC status (`RESOURCE_EXHAUSTED` `rate_limit`, `DEADLINE_EXCEEDED` `timeout`, `UNAUTHENTICATED` and `PERMISSION_DENIED` `auth_error`, `INVALID_ARGUMENT` `invalid_request`, `CANCELLED` `cancelled`).

## Amazon Bedrock

`github.com/aws/aws-sdk-go-v2/service/bedrockruntime`, tested with v1.63.1.

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

Point openai-go at the endpoint and name the provider:

```go
client := openai.NewClient(
	option.WithBaseURL("https://openrouter.ai/api/v1"),
	option.WithAPIKey(os.Getenv("OPENROUTER_API_KEY")),
	rudderopenai.WithTracking(rudderopenai.WithProvider("openrouter")),
)
```

- `openrouter`, `litellm` and `gateway` are routers: a served model with a vendor prefix (`openai/gpt-4o`) is reported under that vendor (`openai`), with the full name in `model`, and `requested_model` holds the requested model when another one served the call.
- Cost: `usage.cost` (OpenRouter, LiteLLM, in a body or a stream chunk) or the LiteLLM proxy's `x-litellm-response-cost` header is the call's cost, ahead of registered prices. The proxy's `x-litellm-model-name` header is the served model.
- Azure OpenAI: `WithProvider("azure.ai.openai")` plus [`WithAzureDeploymentModels`](configuration.md#pricing) for pricing. Other labels (`vllm`, `groq`, ...) are sent as given.

## Streaming and cancellation

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

openai-go, anthropic-sdk-go and genai retry inside the client, below the tracking middleware or transport, so each attempt is recorded as its own call: a failed attempt is a failed call (in `llm_call_count` and the entry's `error_count`), and a successful retry makes the answer a success because the last call decides. The AWS SDK retries below the Bedrock tracker, so a Bedrock operation is recorded once, with the final outcome. `ClassifierClient` calls disable retries.

## Classifier clients

`rudderopenai.ClassifierClient(client openai.Client, model string)` and `rudderanthropic.ClassifierClient(client anthropic.Client, model string)` turn your own client into a `rudderstackai.ClassifierClient` for [`WithClassifier`](configuration.md#classifier-and-evaluator). Their calls are never tracked, also through a client with `WithTracking`. For Gemini or Bedrock, implement `ClassifierClient` yourself.

## Manual

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
| `OutputText`                                      | Answer text for the classifier and evaluator; never sent                                                  |
| `StartedAt`, `EndedAt`                            | Accepted; no property is derived from them yet                                                            |

`ToolCall`: `ID` (merges repeated reports), `Name` (flat `mcp__server__tool` names are split), `MCPServer`, `ParentTool`, `AgentName`, `Error` (`*bool`), `ErrorType`, `Duration` (`*time.Duration`, measured only), `Args` and `Result` (given to declared tool rules, never sent). `ToolResult`: `Error`, `ErrorType`, `Duration`, `Result`. A failure from any report sticks; the first measured duration and the first result win.

An agent that calls MCP servers can join each `mcp_tool_called` to its answer by sending `in.ID()` as `com.rudderstack/interaction_id` in the request `_meta` ([mcp.md](mcp.md#joining-the-calling-agent)); no Go integration adds it for you.

## Limits and differences from Node.js

- Integrations: OpenAI, Anthropic, Google Gen AI and Bedrock clients only. There are no LangChain, LangGraph, OpenAI Agents SDK, Claude Agent SDK or Vercel AI SDK integrations, no OpenTelemetry span processor or exporter, and no HTTP middleware package: pass the request to `WithValues` or `InteractionOptions.Request`.
- Retries of openai-go, anthropic-sdk-go and genai are recorded per attempt ([Retries](#retries)); Node.js wraps the SDK method and sees one call.
- Streams must be read inside the interaction; there is no `bind`. `client_disconnected` and `paused` are set only through `EndOptions`.
- No `streamUsage` option: streamed Chat Completions always ask for usage, with the fallback described above.
- Gemini API and Vertex AI prices are separate per provider label; in Node.js a price for either label applies to both.
- Bedrock stream read-ahead can leave the last one or two decoded events of an early-closed stream uncounted.
- `Flush` cannot send analytics-go's queue (it has no flush method): close the client before exiting.
- Debug output goes through `log/slog` at debug level; there is no environment variable.
- Not tracked: OpenAI background responses that are only queued, embeddings, and every non-generation endpoint.
