# OpenTelemetry span input

The optional `github.com/rudderlabs/ai-sdk-go/rudderotel` module consumes spans your application's
instrumentation already creates. Each finished AI span becomes one `ai_otel_span` RudderStack
track event. `properties.otlp` is a complete OTLP/JSON `ExportTraceServiceRequest` containing that
span, forwardable to `/v1/traces`. The core module has no OpenTelemetry dependency.

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
`openinference.span.kind`, or legacy `ai.operationId`. Custom export filtering does not change
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
| Older OpenLLMetry | `llm.request.type = chat` or `completion` | none |

Agent, workflow, embedding, and other operations do not count. Provider, response/request model,
tokens, cache usage, finish reason, response ID, agent, output text and time to first chunk follow
the GenAI, OpenInference and OpenLLMetry mappings. Cost uses registered core prices. ERROR spans
record failed calls: HTTP status, `error.type`, or the last exception type determines their error
category. Tool attributes supply ID, name, MCP server, parent, measured duration, and outcome.
Arguments/results only reach declared tool-rule callbacks.

The core merges wrapper/span calls with the same provider response ID, or the closest overlapping
call from the other source with matching provider/model, preserving wrapper usage. A tool span
without an ID settles a pending matching named wrapper tool, following Node's behavior. Span-only
tool executions still count individually. Opt-in captured response text comes from the last
successful model span when End supplies no `ResponseText`; prompts never come from spans.

## Privacy and size

Content capture is off by default. Export strips GenAI input/output messages, instructions, tool
arguments/results, retrieval content and legacy prompt/completion prefixes; OpenInference inputs,
outputs, messages, prompt templates, retrieval/embedding/reranker documents; Vercel content fields;
and OpenLLMetry entity input/output. It drops `gen_ai.choice`,
`gen_ai.client.inference.operation.details`, and `gen_ai.<role>.message` events. Other events keep
their non-content attributes. These lists match Node's tracing constants.

With capture enabled, content strings and string arrays, and all string attributes of content
events, pass through the core's redaction and custom hook. Withheld values are removed; arrays
are removed if any element is withheld. Header attributes (`ai.request.headers.*`,
`http.request.header.*`, `http.response.header.*`), client addresses, user agents, and full
URL/path/query attributes are always stripped from span, resource, scope, link, and event attributes.
`http.route` and identity attributes remain. `ai.response.providerMetadata` counts as content.

Content stripping and redaction apply to span, resource, scope, link, and event attributes.
Go deliberately goes beyond Node/Python parity here: their resource, scope, and link attributes
currently bypass these privacy protections.

Oversized events repeatedly shorten the longest OTLP attribute string in resource, span, event,
then link traversal order. Cuts preserve UTF-8, retain at least 256 bytes before the
`...[truncated]` suffix, and set `truncated: true`. An event still too large is dropped with a
diagnostic. The budget measures the Go analytics message, whose timestamp field is
`originalTimestamp`; the data plane normalizes it to `timestamp`. OTel's Go attribute types are
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
