# OpenTelemetry span input

Run `go run ./examples/tracing --check` from the repository root. It uses the real OTel SDK and
`ruddertest.Recorder`, needs no credentials, and verifies the span's OTLP payload, product-call
totals, and interaction/trace joins.

Without `--check`, it appends events to `tracing-events.jsonl` (or `--output <path>`). Replace the
recorder with your analytics-go client to send events to RudderStack. Your instrumentation
creates the model spans; the SDK consumes them. See [tracing](../../docs/tracing.md).
