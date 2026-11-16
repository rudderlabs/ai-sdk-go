package rudderotel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// spanNames lists the exported ai_otel_span names.
func spanNames(events []object) []string {
	out := []string{}
	for _, e := range spanEvents(events) {
		out = append(out, e["properties"].(object)["span_name"].(string))
	}
	return out
}

func modelSpan(ctx context.Context, tracer trace.Tracer, name string) trace.Span {
	_, s := tracer.Start(ctx, name, trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	return s
}

func countName(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

// endOnly is a processor that never classifies a span at start but still ends it: one whose
// recognition differs from another RudderStack processor's.
type endOnly struct{ sdktrace.SpanProcessor }

func (endOnly) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// Coverage is shared: when any RudderStack processor covers a span, every one drops it.
func TestSpanCoverageIsSharedAcrossProcessors(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run("reversed="+strconvBool(reversed), func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			r, events := recorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r)); err != nil {
				t.Fatal(err)
			}
			full, err := NewSpanProcessor(Options{Analytics: r, Simple: true})
			if err != nil {
				t.Fatal(err)
			}
			blind, err := NewSpanProcessor(Options{Analytics: r, Simple: true})
			if err != nil {
				t.Fatal(err)
			}
			processors := []sdktrace.SpanProcessor{full, endOnly{blind}}
			if reversed {
				processors = []sdktrace.SpanProcessor{endOnly{blind}, full}
			}
			opts := []sdktrace.TracerProviderOption{}
			for _, p := range processors {
				opts = append(opts, sdktrace.WithSpanProcessor(p))
			}
			provider := sdktrace.NewTracerProvider(opts...)
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			configureSpans(t)
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			started := time.Now()
			wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
			modelSpan(wrapped, provider.Tracer("app"), "provider").End()
			hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
			i.End(rudderstackai.EndOptions{})
			if names := spanNames(events()); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
				t.Errorf("spans %v, want the SDK span alone", names)
			}
			if n := coveredCalls.n.Load(); n > coveredRecent {
				t.Errorf("%d covered entries retained", n)
			}
		})
	}
}
