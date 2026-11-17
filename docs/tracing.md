# Tracing

The optional `github.com/rudderlabs/ai-sdk-go/rudderotel` module consumes spans your application's
instrumentation already creates. Each finished AI span becomes one `ai_otel_span` RudderStack
track event. `properties.otlp` is a complete OTLP/JSON `ExportTraceServiceRequest` containing that
span, forwardable to `/v1/traces`. The core module has no OpenTelemetry dependency.

## Choose a setup

| Setup                  | Turn on                                                     | You get                                                                                         |
| ---------------------- | ----------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| Product only (default) | Provider wrappers inside an interaction                     | `ai_response_received` and the other product events; no spans                                  |
| Traces only            | Your OpenTelemetry instrumentation and `rudderotel`         | One `ai_otel_span` per AI span; no product events unless you set `ProductEvents: true`          |
| Both                   | `rudderstackai.WithEmitSpans(true)` with your wrappers      | Product events plus one `ai_otel_span` per model call and manual span; no OpenTelemetry needed |

```go
// Product only
err := rudderstackai.Configure(rudderstackai.WithAnalytics(client))
llm := openai.NewClient(rudderopenai.WithTracking())
err = rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID},
    func(ctx context.Context, _ *rudderstackai.Interaction) error {
        _, err := llm.Chat.Completions.New(ctx, params)
        return err
    })

// Traces only: keep your instrumentation; it creates the spans.
provider, err := rudderotel.RegisterTracing(rudderotel.Options{Analytics: client})

// Both: one option on the product setup
err = rudderstackai.Configure(rudderstackai.WithAnalytics(client), rudderstackai.WithEmitSpans(true))
```

Already exporting OpenTelemetry spans and adding `WithEmitSpans(true)`? Keep the processor: each
model call is still sent once ([one span per call](#one-span-per-call-with-the-span-processor)).
Examples: [`openai`](../examples/openai) (product only; `EMIT_SPANS=1` adds spans) and
[`tracing`](../examples/tracing) (traces only).

## Wrapper and manual spans

Enable `rudderstackai.WithEmitSpans(true)` to emit one `ai_otel_span` per wrapped provider call,
including errors and streamed calls. The option defaults to false and is independent of
`WithLLMCallEvents(true)`. Wrapper spans use `gen_ai.operation.name: chat`,
`rudderstack.span_source: wrapper`, interaction identity, provider/model/response/usage attributes,
and status/error metadata. `context.wrapperLibrary.integration` is `tracing`.

```go
err := rudderstackai.Configure(
    rudderstackai.WithAnalytics(client),
    rudderstackai.WithEmitSpans(true),
)
if err != nil { return err }
ctx, span := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{
    Name: "lookup", Operation: "retrieval",
    Attributes: map[string]any{"gen_ai.retrieval.query.text": query},
})
result, err := retrieve(ctx, query)
span.SetAttributes(map[string]any{"gen_ai.retrieval.documents": result})
span.End(err)
return err // the original error, unchanged
```

`StartSpan(ctx, SpanOptions) (context.Context, *Span)` supports `execute_tool`, `retrieval` and
`invoke_agent`. Manual spans use `rudderstack.span_source: manual`. The handle exposes immutable
`TraceID` and `SpanID`, `SetAttributes(map[string]any)` and idempotent `End(error)`. When disabled,
the returned context is unchanged and the handle is a no-op. Pass the returned context to nested
work; the nearest active SDK span becomes its parent only within the same interaction. Ended
spans are skipped. Provider middleware makes its wrapper span active on the context passed
downstream; independent concurrent requests and streams keep independent contexts. No interaction
root span is created.

A root span gets a random nonzero W3C trace ID (32 lowercase hex digits), shared per interaction
and saved as `sdk_trace_id` across pause/resume. The application's `TraceID`, `SetTraceID` and
`WithTraceIDFrom` continue to control the product interaction's `trace_id` independently. A nested
span inherits its parent's trace. Importing `rudderotel` registers an optional bridge that lets an
SDK span inherit an active OTel trace, parent span ID and trace state when there is no active SDK
parent belonging to the same interaction; the core still has no OTel dependency. A new interaction
or a task with `NoParent: true` uses the active OTel parent, or its own fresh interaction trace.
Span identity is captured from the owning interaction when the span starts. Standalone manual
spans capture identity, track context and the analytics client from their start context;
without identity they use the product-event fallback `anonymousId: "ai_system"`.
A new span ignores an ended interaction and its spans as owners or parents, and captures the
current `WithValues` identity and analytics client. A call settling after
its interaction ended still emits its span with the original interaction, trace and parent; its late
product call stays dropped.

Accepted call admission and totals finish before span transport. Spans go directly to the
analytics client's `Enqueue`, using the same normalized OTLP mapper, masking, sensitive attribute
stripping and truncation as the OTel exporter; product-event sampling does not apply. A transport
or `WithOnError` failure cannot undo admission. Span setup, capture, normalization and transport
failures are contained and reported through `WithOnError` or warnings; provider work continues.
SDK retries and usage-parameter fallback share one
span with duration covering the retries. Raw HTTP response access uses the same middleware and
emits once. Streaming spans finish once on completion, early close, cancellation or errors.
Ending an interaction leaves an unread wrapper stream pending until close, cancellation, or flush.
Context cancellation finishes even an unread stream's
span and records the cancelled call, including when the provider decoder is blocked.
With spans disabled, product bookkeeping follows the provider's read/Close path. At the latest, core `Flush` or `Shutdown` finishes every still-pending wrapper span exactly once
as unread/cancelled, with unknown usage, including a streaming response that arrives after its
interaction ended and is never read, closed or aborted. Manual spans remain caller-owned.

With `WithLLMCallEvents(true)`, retained call events carry the wrapper's `span_id`; delayed
pending events preserve it through pause/JSON restoration. Overflow aggregates omit it. Two
separate invocations returning the same response ID or reusing an error value emit two spans and the single call event
keeps the first invocation's ID. Span content follows `WithCaptureContent` even with call events
off, and captured call content is masked once. Typed containers and structs are walked directly, without invoking custom serializers that could
encode bytes before inspection: binary
payloads are stripped, numeric maps are preserved, primitive map keys become strings, other keys
are dropped, cycles are dropped, and other unrepresentable list entries keep null placeholders.

### One span per call with the span processor

When `WithEmitSpans(true)` and a `rudderotel` span processor (`NewSpanProcessor` or
`RegisterTracing`) both see a model call, the SDK span is the one sent when it has an analytics
client and its mandatory core encodes as JSON and fits the span size budget with room for
completion timing and status. A message context that JSON cannot encode (a NaN or infinite
number, an invalid `json.Number`) never fits. Otherwise the OpenTelemetry span exports normally,
and the SDK span follows its ordinary delivery path.

- An OpenTelemetry model-call span (the conventions in
  [product events from spans](#product-events-from-spans): GenAI `chat` / `text_completion` /
  `generate_content`, OpenInference `LLM`, older OpenLLMetry `llm.request.type`) that starts
  inside any active wrapper call ancestor in the current interaction is dropped by every
  `rudderotel` processor, using a mark they share. Manual tool, retrieval and agent middleware do
  not hide the call; the ancestry stops at the interaction boundary. Other processors and
  exporters receive it unchanged. Exported descendants of a covered OpenTelemetry model span keep
  their span IDs in the SDK owner's trace; direct children use the SDK owner as parent, and deeper
  parent links stay intact. `Filter`, `ResolveIdentity` and `ExtraPropertiesFrom` get your
  original span, with its own trace, span and parent IDs; the exported IDs are applied after them.
  Your APM spans keep the original OpenTelemetry data. Span-built product events still count the
  call once.
- If an earlier processor already sent a child under an OpenTelemetry model span's original IDs
  before RudderStack observed that parent, the original parent is kept as well as the SDK span.
  Buffered children still resolve to the SDK owner at export.
- For an active, still-recording model-call span a processor observed outside an SDK call, in the
  same open interaction, the first wrapper call that starts directly inside it takes its place.
  The SDK span adopts its trace ID, span ID and parent span ID, and fixes the interaction's SDK
  trace, including pause snapshots. It is sent normally, through the SDK's own path;
  `ai_llm_response_received.span_id` carries the adopted ID. Later wrapper calls inside that span
  are ordinary children of that ID. Every `rudderotel` processor drops the replaced span when it
  ends, synchronously, by a mark they share, including one registered after the span started.
  Filters, sampling, batching, analytics-client failures and processor shutdown do not decide
  whether the call's span is sent, and a stream read after the OpenTelemetry span ended still
  sends the adopted span.
- An outer model-call span with no wrapper call inside is exported normally. A newer SDK span in
  between prevents adoption: the most recently started active SDK span decides.
- An SDK span that replaced or covered an OpenTelemetry span sheds optional fields in this order
  after ordinary string truncation: (1) content span, resource and scope attributes and events
  identified by [`span-content-keys-v0.1.json`](../spec/fixtures/span-content-keys-v0.1.json), OTLP
  `status.message` and property `status_message`; (2) all other optional span, resource and scope
  attributes, events, links, optional `gen_ai` entries and properties, and span
  `droppedAttributesCount`, `droppedEventsCount`, `droppedLinksCount`. Each group sheds the
  largest compact-JSON UTF-8 value first; ties follow the
  [shared delivery rule](../spec/fixtures/FIXTURE-FORMAT.md#sdk-span-delivery-budget). The message
  identity and context, span IDs, name, kind, timing and status, session, interaction and
  conversation IDs, the OTLP envelope and scope, and the core GenAI summary are never shed. Core
  span attribute keys, when present at admission, are `gen_ai.operation.name`,
  `gen_ai.provider.name`, `gen_ai.system`, `gen_ai.request.model`, `gen_ai.conversation.id`,
  `rudderstack.interaction_id`, `rudderstack.span_source`, `user.id` and
  `rudderstack.anonymous_id`. String shortening or shedding sets `truncated: true`; an ordinary
  oversized SDK span that replaced or covered nothing still drops.

The trade-off: RudderStack gets the SDK span, with its attributes and timing, under the
OpenTelemetry span's IDs, not the attributes and timing your application put on its span.
Processors that are not RudderStack's (your APM exporter, for example) receive your original span
unchanged: nothing from the SDK is copied onto it, and the processor's usual identity attributes
still apply.

Matching uses context nesting only, never times or model names. Limits:

- When RudderStack processors replace an OTel model span, the SDK span is the one kept; if the SDK's own client fails to deliver, that span is lost, as the call's product events are.
- Instrumentations that never put their span on the context passed downstream cannot be matched.
- The bare `Exporter` has no start hook; use the processor.
- A span is classified when it starts: model-call attributes added later cannot enable adoption.
  A recording span the sampler does not sample is eligible too.
- A span that already ended, an ended interaction, and a span that started in another interaction
  never adopt.
- Go has no framework integrations, so no integration model spans take part.
- Dedup tracks at most `Limits.MaxMapEntries` open model-call spans (default 10,000). A
  model-call span that starts while that many are open, for example behind spans that never end,
  is neither covered nor adopted and exports as without dedup (`limit_reached`). An entry goes
  when its span ends or every processor that saw it shuts down; descendants of an already covered
  span are tracked until they end, so its tree stays intact.
- With two copies of the tracing module (for example a vendored fork under another import path),
  replacement marks are not shared between copies, so an adopted span can be exported twice;
  register processors from one copy.

## Framework trees with manual spans

LangChain, OpenAI Agents SDK, Claude Agent SDK and Vercel AI SDK framework integration spans
(`rudderstack.span_source: integration`) are unavailable in Go because Go has no integrations
for those frameworks. This is a platform limit. Build the same agent/tool/model tree with
manual spans and wrapped provider calls instead. Enable `WithEmitSpans(true)` and pass the
context returned by `StartInteraction` and each `StartSpan` into every goroutine and call:

```go
// client is an OpenAI client configured with rudderopenai.WithTracking().
err := rudderstackai.RunInteraction(ctx, opts, func(ctx context.Context, in *rudderstackai.Interaction) error {
    ctx, agent := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{
        Name: "assistant", Operation: "invoke_agent",
    })
    var wg sync.WaitGroup
    errs := make(chan error, 2)
    for _, name := range []string{"search", "lookup"} {
        wg.Go(func() {
            toolCtx, tool := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{
                Name: name, Operation: "execute_tool",
            })
            // Use toolCtx for retrieval spans and other nested work too.
            stream := client.Chat.Completions.NewStreaming(toolCtx, params)
            for stream.Next() {
                send(stream.Current())
            }
            err := stream.Err()
            if closeErr := stream.Close(); err == nil {
                err = closeErr
            }
            tool.End(err)
            errs <- err
        })
    }
    wg.Wait()
    var result error
    for range 2 {
        result = errors.Join(result, <-errs)
    }
    agent.End(result)
    return result
})
```

Import `context`, `errors` and `sync` for this pattern. `send` must support concurrent calls.
The two manual tool spans parent to the manual agent; each streamed wrapper span parents to
its own tool. All share the interaction and trace. Keep parents open until workers and streams
finish. An `errgroup.WithContext(ctx)` works the same way: pass its derived context into workers.

Provider middleware records a failed span when a downstream handler panics, then re-panics with
its original value unchanged. Manual spans use an explicit lifecycle: call `End(err)`; errors
record their Go concrete type, and error text follows the content policy. Raw responses that the
middleware cannot map finish only their span with HTTP status, without parsed usage or content
or an invented product call. Raw access to a mapped JSON response still supplies the usage and
content the middleware parses. Invalid JSON, JSON `null` and response extraction failures finish
with error status while preserving product-event behavior. Normal wrapper completion
uses the same attribute set as Node.

## Go platform limits

- SDK span parents propagate through explicit `context.Context`, rather than Node's ambient
  asynchronous context. Pass the context returned by `StartSpan` to nested work.
- OpenAI and Anthropic integrations run at HTTP middleware, below SDK response decoding.
  Wrapper failures carry the normalized error category, matching Node; the provider SDK
  constructs its Go error after middleware returns. Decoding failures after a successful HTTP
  response cannot change the already completed HTTP span, except invalid JSON and JSON `null`
  are detected by the middleware and finish with error status.
- Google GenAI's Go SDK does not execute Node's automatic function calling (AFC) callback loop or
  expose its `automaticFunctionCallingHistory`. Each Go `GenerateContent`/stream request emits
  one span. Node AFC emits one span per outer request, joining only the last call. Applications
  managing a Go tool loop make separate requests; pass the manual span context explicitly to
  tools and subsequent requests.
- LangChain, OpenAI Agents SDK, Claude Agent SDK and Vercel AI SDK framework integrations have
  no Go wrapper. Node supports all four; Python supports the first three. Their integration
  fixture cases are explicitly skipped in Go. Bound-stream completion APIs also have no Go wrapper.
- HTTP middleware has no hook for the outer SDK method's promise settlement. Retry grouping uses
  per-invocation request identity, retry headers and response-body probes. With spans enabled,
  every failed attempt has a bounded one-second retry grace, even without context cancellation.
  This settles calls with no configured retries or an exhausted retry budget. Ending an interaction
  while an attempt is held, or a retry arriving after that grace, can split spans. These boundary
  cases cannot reproduce Node's
  outer-method duration and late-settlement behavior from HTTP middleware alone.
- Go has no native tuple or set type; portable tuple/set fixtures use arrays/slices. Foreign
  runtime exception class fixtures use native Go errors. `MaxTurnsExceededError`,
  `LengthFinishReasonError` and `ContentFilterFinishReasonError` have no existing Go Agent SDK or
  response-parser exception equivalent and are skipped by the classification fixture driver.
- An unread wrapper stream settles on close, cancellation, or flush (`Flush` or `Shutdown`). Go has
  no deterministic iterator finalization when an open stream is simply abandoned.

## Setup

Add the processor to your existing OTel provider:

```go
import (
    "context"

    sdktrace "go.opentelemetry.io/otel/sdk/trace"

    "github.com/rudderlabs/ai-sdk-go/rudderotel"
)

// client is your analytics-go v4 client.
processor, err := rudderotel.NewSpanProcessor(rudderotel.Options{Analytics: client})
if err != nil { return err }
provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
// Register your provider and instrumentation as usual.
// Before returning from a serverless invocation:
if err := provider.ForceFlush(context.Background()); err != nil { return err }
// Before the process exits:
if err := provider.Shutdown(context.Background()); err != nil { return err }
// Close the client you own afterwards.
```

For one-call global setup, use
`rudderotel.RegisterTracing(options, providerOptions ...sdktrace.TracerProviderOption)`.
It returns `(*sdktrace.TracerProvider, error)` and refuses to replace an existing global provider,
including an explicitly registered noop provider. Call it during application startup. Resource,
sampler, and additional span processors use normal `sdktrace` provider options. When you already
own a provider, use `NewSpanProcessor`. All propagation uses `context.Context`; pass the context
returned by `StartInteraction` and `Tracer.Start` to downstream work.

`NewExporter(options) (*Exporter, error)` implements `sdktrace.SpanExporter` for custom processor
arrangements. `SpanToTrack(span, options) (*analytics.Track, error)` exposes mapping without the
export filter. A nil track means the span could not fit its budget. Using the exporter directly
does not add interaction identity or record product calls; those belong to `SpanProcessor`.

Exported function signatures (types below are from `rudderotel`):

```go
func NewSpanProcessor(options Options) (*SpanProcessor, error)
func NewExporter(options Options) (*Exporter, error)
func RegisterTracing(options Options, providerOptions ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error)
func SpanToTrack(span sdktrace.ReadOnlySpan, options Options) (*analytics.Track, error)
func IsAISpan(span sdktrace.ReadOnlySpan) bool
```

`Options` configures these entry points; `Identity{UserID, AnonymousID string}` is returned by the
identity resolver. `SpanProcessor` implements `OnStart(context.Context, sdktrace.ReadWriteSpan)`,
`OnEnd(sdktrace.ReadOnlySpan)`, `ForceFlush(context.Context) error`, and
`Shutdown(context.Context) error`. `Exporter` implements
`ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error`, `Shutdown(context.Context) error`,
and also offers `ForceFlush(context.Context) error`. Constants expose `DefaultEventName`,
`SchemaVersion`, `DefaultMaxEventBytes`, `AnonymousIDAttribute`, and `InteractionIDAttribute`.

## Options

| `rudderotel.Options` field | Default | Behavior |
| --- | --- | --- |
| `Analytics` | core's configured client | Required if no client was configured. |
| `CaptureContent *bool` | core's `WithCaptureContent`, read at export | An explicit pointer wins, including false. |
| `ContentCapture *rudderstackai.ContentCapture` | core's `WithContentCapture`, read at export | Span content per kind (`Spans: &SpanCapture{Messages, ToolCalls, HTTP}`); its keys win over `CaptureContent`. See [Content capture](configuration.md#content-capture). |
| `ExportAllSpans` | false | Export HTTP/database spans too. |
| `Filter func(sdktrace.ReadOnlySpan) bool` | `IsAISpan` | Replaces the default filter and `ExportAllSpans`. |
| `ResolveIdentity func(sdktrace.ReadOnlySpan) Identity` | attribute/resource identity | Empty identity or a panic falls back to defaults. |
| `MaxEventBytes` | 32000 | UTF-8 budget for the serialized Go track event; negative values are rejected. |
| `EventName` | `ai_otel_span` | Custom event name. |
| `ExtraProperties` / `ExtraPropertiesFrom` | none | Map or span callback; the callback wins. SDK property names cannot be overridden. |
| `Simple` | false | Synchronous export at End; otherwise OTel batch processing. |
| `BatchOptions` | OTel defaults | `[]sdktrace.BatchSpanProcessorOption` tuning. |
| `PropagateIdentity *bool` | true | Inherit parent `user.id` and `session.id`. |
| `ProductEvents` | false | Record model/tool spans in their open interaction. |
| `LinkTraceID` | false | Link the interaction to the first span started inside it, including non-AI spans. |

The default AI-span filter keeps any span with a `gen_ai.*` attribute,
`openinference.span.kind`, `traceloop.span.kind = "tool"`, or legacy `ai.operationId`. Custom export filtering does not change
which spans count as product calls. Older OpenLLMetry spans with only `llm.request.type` can
count as product calls while failing the default export filter, matching Node and Python.

## Identity and joins

OnStart adds missing `user.id`, `session.id`, `rudderstack.anonymous_id`, `gen_ai.conversation.id`, and
`rudderstack.interaction_id` from the active interaction; outside an interaction, it uses
`WithValues` and its cookie identity, including `session.id`. Instrumentation's own attributes
win. Parent `user.id` / `session.id` fill missing context values. System runs (`RunTask`) inherit
no personal identity from parent spans.

Identity stays off non-AI spans, so other exporters see the original HTTP/database spans. It is
held aside for AI descendants. If instrumentation adds its AI attributes after Start, RudderStack
uses a tagged copy of the ended snapshot, following Python's behavior. Go OTel's OnEnd supplies
an immutable snapshot. The processor retains active span state until End; a bounded cache
(`Limits.MaxMapEntries`) holds ended-parent identity without retaining span objects.

The event's `interaction_id` and `conversation_id` join it to product events. `LinkTraceID` fills
an unset interaction trace ID; an explicit `TraceID` wins, and a later `SetTraceID` replaces the
linked value. `WithTraceIDFrom(rudderotel.TraceID)` remains available to read a span already
active when an interaction starts, independently of span input.

Default exported identity reads each key from the span, then its resource:

- User: `user.id`, then `traceloop.association.properties.user_id`.
- Anonymous: `rudderstack.anonymous_id`, then `session.id`, then
  `traceloop.association.properties.session_id`, then the trace ID.

## Product events from spans

```go
processor, err := rudderotel.NewSpanProcessor(rudderotel.Options{
    Analytics: client, ProductEvents: true, LinkTraceID: true,
})
// Add processor to the application's provider.
ctx, interaction := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{
    UserID: userID, ConversationID: conversationID,
})
// Call your instrumented model/tool with ctx. End its spans before ending the interaction.
interaction.End(rudderstackai.EndOptions{})
```

Spans must name an open interaction through `rudderstack.interaction_id`. Spans outside one,
or ending after its final End, do not produce product events. An in-flight span retains its owner
if the interaction pauses before the span ends, and accepted work resumes it, following Python.
The interaction's explicit/context client wins; otherwise it adopts tracing's client.

The first convention present decides the role:

| Convention | Model call | Tool call |
| --- | --- | --- |
| GenAI | `chat`, `text_completion`, `generate_content` operations | `execute_tool` |
| OpenInference | `openinference.span.kind = LLM` (case insensitive) | `TOOL` |
| Traceloop / OpenLLMetry tools | none | `traceloop.span.kind = "tool"` (`traceloop.entity.name`) |
| Older OpenLLMetry | `llm.request.type = chat` or `completion` | none |

Agent, workflow, embedding, and other operations do not count. Provider, response/request model,
tokens, cache usage, finish reason, response ID, agent, output text and time to first chunk follow
the GenAI, OpenInference and OpenLLMetry mappings. Cost uses registered core prices. ERROR spans
record failed calls: HTTP status, `error.type`, or the last exception type determines their error
category. Tool attributes supply ID, name, MCP server, parent, measured duration, and outcome.
Arguments/results only reach declared tool-rule callbacks.

Span mapping rules (SDK-6898; shared cases in [`tracing-product-events-v0.1.json`](../spec/fixtures/tracing-product-events-v0.1.json)):

- Nested model calls: skip a model-call span only when a retained descendant model-call span in the same trace carries a response ID or usage attributes, and either has a response ID or shares any exact response/request model name with an overlapping time window (inclusive endpoints). The parent must have no response ID or the same response ID as that descendant; different response IDs keep both calls even when the model and time window match. A descendant with no usage attributes and no response ID never suppresses its richer parent; explicit zero token counts are usage evidence. A failed parent is skipped only for a failed descendant with the same model and overlapping window; a successful fallback never removes its failure. Keep the descendant. This includes ADK Go `generate_content` with a direct `anthropic.messages.create` child and descendants through non-model spans. Nesting alone is insufficient: different or unknown models without a response ID remain separate calls.
- OpenInference prep spans: skip an `openinference.span.kind = "LLM"` model-call span only if it has no token-usage attributes in any supported convention (including input, output and cache counts; zero counts are usage), no response ID, no output messages or text (including tool-only output), and status other than `ERROR`; another unrelated model-call span with usage (neither ancestor nor descendant) must share its interaction and trace and start between this span's start minus **1 ms** and the interaction end, inclusive. This covers overlapping calls and later LlamaIndex `_prepare_chat_with_tools` / `achat` root siblings in either end order. A lone span, a failed span, or usage in another trace/interaction cannot trigger this rule.
- Counting at End: every model-call span records immediately, retaining internal trace/span IDs, parent/ancestor IDs, raw request/response model, response ID, timing, usage/output presence, ERROR status and OpenInference LLM kind. These tags never appear in product or exported span events. After interaction finalizers, the interaction's synchronous end path applies both rules once to recorded span calls, before pending call events, totals, costs, breakdowns, primary model and answer selection. Skipped calls cannot provide evidence. Cycles are handled conservatively so a group retains a call. A call whose event was already emitted by an earlier adapter match is kept and counted, even if later spans make it redundant; published call events must agree with answer totals. Remaining pending calls close skipped positions in `call_index`. Processor/SDK flush and processor shutdown do not settle or hold model calls. At `maxCallsPerInteraction`, folded calls count conservatively and cannot provide individual span evidence; non-model ancestry is bounded by `maxMapEntries`.
- Pause/resume: pause finalizes and filters its segment before publishing its totals and pending calls, then clears that segment's calls and ancestry. Its calls do not count again after resume. Delivery callbacks can nevertheless record calls for the next segment before the pause snapshot is returned. Those pending calls carry the same internal tags, including resolved ancestors through non-model spans, in `pending_calls[].span_source`; JSON restoration retains and validates them, so End can apply the rules across resume. Older snapshots without tags count conservatively.
- Cache writes: `gen_ai.usage.cache_write.input_tokens` takes precedence over `gen_ai.usage.cache_creation.input_tokens`, then `gen_ai.usage.cache_creation_input_tokens`, then `llm.token_count.prompt_details.cache_write`. Use the first valid nonnegative integer, including zero. Map it once to `cache_write_tokens` and apply the same cache-write price as cache creation; never sum aliases.
- Provider API IDs: product calls map only `anthropic.messages` → `anthropic`, `openai.chat` / `openai.responses` → `openai`, and `google.generative-ai` → `gcp.gemini`. The first three are provider IDs in AI SDK v7's `@ai-sdk/anthropic` 4.0.89 and `@ai-sdk/openai` 4.0.103 `src/*-provider.ts`; the Google ID is already documented in [`providers-v0.1.json`](../spec/fixtures/providers-v0.1.json). Apply this explicit allow-list after the existing lowercase step; never split arbitrary names at a dot. `aws.bedrock`, `gcp.vertex_ai`, `gcp.gemini`, `azure.ai.openai`, `azure.ai.inference` and unknown dotted names stay unchanged. Raw OTLP provider attributes and the GenAI provider summary keep the reported label.
- Semantic Kernel: when `gen_ai.response.finish_reasons` is absent, read singular `gen_ai.response.finish_reason`. Strip the prefix through the last dot and lowercase the value: `FinishReason.STOP` / `STOP` → `stop`, `LENGTH` → `length`, `TOOL_CALLS` → `tool_calls`, `CONTENT_FILTER` → `content_filter`, case-insensitively. Unknown suffixes are lowercased too. Existing plural values keep their precedence and spelling.
- Traceloop / OpenLLMetry: after GenAI and OpenInference classification, `traceloop.span.kind = "tool"` identifies one `execute_tool` execution. Read the tool name from `traceloop.entity.name` after `gen_ai.tool.name` / `tool.name`, and arguments/result from `traceloop.entity.input` / `traceloop.entity.output` after the existing tool content keys. Successful and failed executions count in `tool_breakdown`; these entity content keys follow `rudderstackai.WithCaptureContent(true)` and masking. The default `IsAISpan` filter also exports these tools and attaches interaction context when they start.
- Mirascope native ops: parse `mirascope.response.usage` only if it is a JSON string of at most 64 KiB (65,536 UTF-8 bytes). Ignore malformed JSON, non-object JSON and invalid counts without throwing. Mirascope 2.8.2's `serialize_mirascope_usage` writes `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `reasoning_tokens` and `total_tokens`; map only the first four as independent fallbacks after existing GenAI and OpenInference usage keys. Zero counts are valid and existing counts win. Input tokens already include cache reads/writes; do not add them again. Ignore reasoning/total counts rather than deriving missing usage. Cache counts use registered cache pricing.

OpenLLMetry Anthropic instrumentation 0.63.0 can overcount streamed output before emitting the span: it adds cumulative message-delta usage to message-start usage. The mapper copies the output count directly, without summing deltas or total-token attributes. Streamed output from that version can read one token high; it's an upstream issue.

The core merges wrapper/span calls with the same provider response ID, or the closest overlapping
call from the other source with matching provider/model, preserving wrapper usage. A tool span
without an ID settles a pending matching named wrapper tool, following Node's behavior. Span-only
tool executions still count individually. Opt-in captured response text comes from the last
successful model span when End supplies no `ResponseText`; prompts never come from spans.

## Privacy and size

Content capture is off by default. Export strips GenAI input/output messages, instructions, tool
arguments/results, retrieval content and legacy prompt/completion prefixes; OpenInference inputs,
outputs, messages, prompt templates, retrieval/embedding/reranker documents; Vercel content fields;
OpenLLMetry entity input/output; Pydantic AI messages, final results, tool arguments/responses,
deferral metadata, decision state, questions and answers, request parameters and model and
embedding settings (its default `include_content=True`), plus `prompt` and `inputs` on spans from
the `pydantic-ai` instrumentation scope only; `gen_ai.tool.definitions`; and Mirascope
response messages and content, traced arguments and outputs and annotation reasoning, plus its
`error.message` on spans from the `mirascope.llm` and `mirascope.ops` scopes, masked like
`exception.message` (a custom Mirascope tracer name doesn't match); and Eve's `ai.response.tool_calls`,
plus `agent.session.title` and `agent.channel.delivery.input` on spans from its `eve.agent` scope. Pydantic AI's
`metadata` and `gen_ai.agent.description` are written by your app and exported as they are.
It drops `gen_ai.choice`,
`gen_ai.client.inference.operation.details`, and `gen_ai.<role>.message` events. Other events keep
their non-content attributes. These lists match Node's tracing constants. Go also strips error
text: span status descriptions (including the summary `status_message`) and `exception.message`
and `exception.stacktrace` attributes on exception events. Exception types and status codes remain.
Node/Python error-text protections follow in SDK-6802.

With capture enabled, content strings and string arrays, and all string attributes of content
events, plus error text, pass through the core's redaction and custom hook. Withheld values are
removed; arrays are removed if any element is withheld. Header attributes (`ai.request.headers.*`,
`http.request.header.*`, `http.response.header.*`), client addresses, user agents, and full
URL/path/query attributes are always stripped from span, resource, scope, link, and event attributes.
`http.route` and identity attributes remain. `ai.response.providerMetadata` counts as content.

Content stripping and redaction apply to span, resource, scope, link, and event attributes.
Go deliberately goes beyond Node/Python parity here: their resource, scope, and link attributes
currently bypass these privacy protections.

Oversized events repeatedly shorten the longest OTLP attribute string in resource, span, event,
then link traversal order. Cuts preserve UTF-8, retain at least 256 bytes before the
`...[truncated]` suffix, and set `truncated: true`. An event still too large is dropped with a
diagnostic. Exporter spans configure this budget with `rudderotel.Options.MaxEventBytes` (32,000 bytes by
default); core spans use the 32,000-byte default. The budget covers the event the SDK builds:
properties, context and identity. The analytics client may add its own
small envelope fields, including type, channel, library, integrations and timestamps. The SDK-built
event uses `originalTimestamp`; the data plane normalizes it to `timestamp`. OTel's Go attribute types are
preserved, including floating-point values. Attribute order can differ across SDKs.

## Lifecycle and diagnostics

`SpanProcessor.ForceFlush(ctx)` waits for pending classifications, exports the batch, and flushes
the configured RudderStack client. `Shutdown(ctx)` does the same before stopping export. Both use one context
deadline (10 seconds when none was supplied), return errors on failure, and never close the app's
client or end interactions. Core `Flush` also reaches the tracing client. SDK classifier and
evaluator calls carry suppressed contexts: their spans are neither recorded nor exported.

Mapping failures and callback panics are contained and reported through the core's `WithOnError`;
enqueue errors become OTel export errors, with the remaining batch still attempted. Size drops
use `log/slog`. Delivery errors after enqueue follow analytics-go's own error handling.

Tested with OTel API/trace/SDK **v1.47.0** and analytics-go **v4.5.1**. The runnable
[example](../examples/tracing) uses a local recorder and needs no credentials:

```sh
go run ./examples/tracing --check
```

## Core integration API

`internal/hook.Tracing` is the integration API for sibling modules under this repository's module
path. It exposes identity/suppression, open-interaction lookup, analytics adoption, measured tool
recording, configured client/content policy, redaction, client registration, and background-work
waiting and client flushing. It contains no OTel types and adds no exported core API. Span LLM calls use the existing
`Interaction.RecordCall(LLMCall)` and trace linking preserves `Interaction.SetTraceID` behavior.
