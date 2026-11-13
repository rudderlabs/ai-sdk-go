// Package rudderotel consumes OpenTelemetry AI spans as RudderStack track events and optionally
// records them as calls of open AI interactions. NewSpanProcessor attaches to an app's provider;
// RegisterTracing creates a global provider, refusing an existing one. TraceID also links an
// interaction to the span already active in its context.
//
//	err := rudderstackai.Configure(rudderstackai.WithTraceIDFrom(rudderotel.TraceID))
package rudderotel

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// TraceID returns the trace ID of the OpenTelemetry span active in ctx, or "".
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

func init() {
	hook.Tracing.ParentContext = func(ctx context.Context) (string, string, string) {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return "", "", ""
		}
		return sc.TraceID().String(), sc.SpanID().String(), sc.TraceState().String()
	}
}
