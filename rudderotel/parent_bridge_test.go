package rudderotel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestSDKSpanInheritsOTelParent(t *testing.T) {
	for _, interaction := range []bool{false, true} {
		t.Run(strconvBool(interaction), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx, parent := provider.Tracer("test").Start(context.Background(), "parent", trace.WithNewRoot())
			defer parent.End()
			var i *rudderstackai.Interaction
			if interaction {
				ctx, i = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
				defer i.End(rudderstackai.EndOptions{})
			}
			childCtx, s := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "child", Operation: "execute_tool"})
			_, nested := rudderstackai.StartSpan(childCtx, rudderstackai.SpanOptions{Name: "nested", Operation: "retrieval"})
			nested.End(nil)
			s.End(nil)
			spans := spanEvents(events())
			if len(spans) != 2 {
				t.Fatalf("spans=%d", len(spans))
			}
			p := spans[1]["properties"].(object)
			if p["trace_id"] != parent.SpanContext().TraceID().String() || p["parent_span_id"] != parent.SpanContext().SpanID().String() {
				t.Fatalf("OTel parent missing: %v", p)
			}
			if q := spans[0]["properties"].(object); q["parent_span_id"] != s.SpanID || q["trace_id"] != s.TraceID {
				t.Fatal("SDK parent precedence lost")
			}
		})
	}
}

func TestSDKSpanInheritsOTelTraceState(t *testing.T) {
	_, _, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	state, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceState: state, Remote: true})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	childCtx, child := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "child"})
	_, nested := rudderstackai.StartSpan(childCtx, rudderstackai.SpanOptions{Name: "nested"})
	nested.End(nil)
	child.End(nil)
	for _, event := range spanEvents(events()) {
		p := event["properties"].(object)
		s := p["otlp"].(object)["resourceSpans"].([]any)[0].(object)["scopeSpans"].([]any)[0].(object)["spans"].([]any)[0].(object)
		if s["traceState"] != "vendor=value" {
			t.Errorf("trace state lost: %v", s["traceState"])
		}
	}
}
