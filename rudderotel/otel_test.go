package rudderotel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestTraceID(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("no span: %q", got)
	}
	tid := trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled})
	if got := TraceID(trace.ContextWithSpanContext(context.Background(), sc)); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("span: %q", got)
	}
}
