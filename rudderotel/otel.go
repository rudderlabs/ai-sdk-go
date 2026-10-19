// Package rudderotel links AI interactions to OpenTelemetry traces: with it, an interaction
// without an explicit TraceID takes the trace of the span active in its context as trace_id.
//
//	err := rudderstackai.Configure(rudderstackai.WithTraceIDFrom(rudderotel.TraceID))
package rudderotel

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// TraceID returns the trace ID of the OpenTelemetry span active in ctx, or "".
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}
