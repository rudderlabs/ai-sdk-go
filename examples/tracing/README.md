# OpenTelemetry span input

The real OTel SDK and `ruddertest.Recorder` verify the span's OTLP payload, product-call totals, and interaction/trace joins. See [setup](../README.md#setup), then run `go run ./examples/tracing --check`.

Without `--check`, it appends events to `tracing-events.jsonl` (or `--output <path>`). Replace the recorder with your analytics-go client to send events to RudderStack.
Your instrumentation creates the model spans. The SDK consumes them. See [tracing](../../docs/tracing.md).
