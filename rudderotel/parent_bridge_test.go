package rudderotel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestForeignSDKParentFallsBackToOTel(t *testing.T) {
	for _, task := range []bool{false, true} {
		for _, wrapper := range []bool{false, true} {
			t.Run("task="+strconvBool(task)+"/wrapper="+strconvBool(wrapper), func(t *testing.T) {
				provider, _, events := testProvider(t, Options{Simple: true})
				if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
					t.Fatal(err)
				}
				ctx, alice := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{AnonymousID: "alice"})
				defer alice.End(rudderstackai.EndOptions{})
				ctx, sdkParent := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "alice"})
				defer sdkParent.End(nil)
				ctx, otelParent := provider.Tracer("test").Start(ctx, "external", trace.WithNewRoot())
				defer otelParent.End()
				check := func(ctx context.Context, i *rudderstackai.Interaction) error {
					if wrapper {
						_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
						hook.Tracing.FinishRaw(s, "openai", "m", 200, time.Now(), nil)
					} else {
						_, s := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "child"})
						s.End(nil)
					}
					p := spanEvents(events())[0]["properties"].(object)
					if p["trace_id"] != otelParent.SpanContext().TraceID().String() || p["parent_span_id"] != otelParent.SpanContext().SpanID().String() {
						t.Fatalf("foreign SDK parent beat active OTel parent: %v", p)
					}
					return nil
				}
				if task {
					if err := rudderstackai.RunTask(ctx, "unrelated", rudderstackai.TaskOptions{NoParent: true, Flush: rudderstackai.Ptr(false)}, check); err != nil {
						t.Fatal(err)
					}
				} else {
					ctx, bob := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "bob"})
					defer bob.End(rudderstackai.EndOptions{})
					_ = check(ctx, bob)
				}
			})
		}
	}
}

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

func TestWrapperOTelParentBridgeRejectsPartialContexts(t *testing.T) {
	state, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		config trace.SpanContextConfig
	}{
		{"empty", trace.SpanContextConfig{}},
		{"trace only", trace.SpanContextConfig{TraceID: trace.TraceID{1}, TraceState: state}},
		{"span only", trace.SpanContextConfig{SpanID: trace.SpanID{2}, TraceState: state}},
		{"remote parent", trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceState: state, Remote: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, events := testProvider(t, Options{Simple: true})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			sc := trace.NewSpanContext(tc.config)
			ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
			ctx, owner := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			defer owner.End(rudderstackai.EndOptions{})
			_, wrapper := hook.Tracing.BeginWrapper(ctx, owner, time.Now())
			// A retry activates its original wrapper in the new attempt's context.
			retryCtx := hook.Tracing.ActivateWrapper(ctx, wrapper)
			_, child := rudderstackai.StartSpan(retryCtx, rudderstackai.SpanOptions{Name: "retry child", Operation: "execute_tool"})
			child.End(nil)
			hook.Tracing.FinishRaw(wrapper, "openai", "m", 202, time.Now(), nil)
			hook.Tracing.FinishRaw(wrapper, "openai", "m", 202, time.Now(), nil)
			spans := spanEvents(events())
			if len(spans) != 2 {
				t.Fatalf("spans=%d, want child and one wrapper", len(spans))
			}
			parent := spans[1]["properties"].(object)
			if sc.IsValid() {
				if parent["trace_id"] != sc.TraceID().String() || parent["parent_span_id"] != sc.SpanID().String() {
					t.Fatalf("valid remote parent lost: %v", parent)
				}
			} else if parent["parent_span_id"] != nil || parent["trace_id"] == sc.TraceID().String() {
				t.Fatalf("partial context became a parent: %v", parent)
			}
			if p := spans[0]["properties"].(object); p["parent_span_id"] != parent["span_id"] || p["trace_id"] != parent["trace_id"] {
				t.Fatalf("reactivation did not give the SDK wrapper precedence: %v", p)
			}
			for _, event := range spans {
				p := event["properties"].(object)
				s := p["otlp"].(object)["resourceSpans"].([]any)[0].(object)["scopeSpans"].([]any)[0].(object)["spans"].([]any)[0].(object)
				if sc.IsValid() {
					if s["traceState"] != "vendor=value" {
						t.Fatalf("remote trace state lost: %v", s)
					}
				} else if _, present := s["traceState"]; present {
					t.Fatalf("partial context leaked trace state: %v", s)
				}
			}
		})
	}
}
