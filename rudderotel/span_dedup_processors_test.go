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

var modelSpanAttr = attribute.String("gen_ai.operation.name", "chat")

// Every processor that ends a replaced or covered span drops it, whether it saw the span start
// or not: one registered after the span started, an adapter that only forwards OnEnd, in any
// order and mix of simple and batch processors. The last processor's end never erases the mark
// a later one needs.
func TestSpanDedupEveryProcessorThatEndsTheSpanDropsIt(t *testing.T) {
	modes := []struct {
		name          string
		first, second bool // simple
	}{{"simple+simple", true, true}, {"simple+batch", true, false}, {"batch+simple", false, true}, {"batch+batch", false, false}}
	for _, attach := range []string{"late", "late-after-two", "end-only-after", "end-only-before"} {
		for _, inner := range []bool{false, true} {
			for _, m := range modes {
				t.Run(attach+"/inner="+strconvBool(inner)+"/"+m.name, func(t *testing.T) {
					_, p1, events := testProvider(t, Options{Simple: m.first})
					configureSpans(t)
					p2, err := NewSpanProcessor(Options{Simple: m.second, Analytics: p1.exporter.client})
					if err != nil {
						t.Fatal(err)
					}
					// p0 is a second processor that saw the start, for "late-after-two".
					p0, err := NewSpanProcessor(Options{Simple: m.second, Analytics: p1.exporter.client})
					if err != nil {
						t.Fatal(err)
					}
					var opts []sdktrace.TracerProviderOption
					switch attach {
					case "late":
						opts = []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(p1)}
					case "late-after-two":
						opts = []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(p0)}
					case "end-only-after":
						opts = []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(endOnly{p2})}
					case "end-only-before":
						opts = []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(endOnly{p2}), sdktrace.WithSpanProcessor(p1)}
					}
					provider := sdktrace.NewTracerProvider(opts...)
					t.Cleanup(func() {
						_ = provider.Shutdown(context.Background())
						_ = p0.Shutdown(context.Background())
						_ = p2.Shutdown(context.Background())
					})
					tracer := provider.Tracer("app")
					ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
					var o trace.Span
					if inner {
						started := time.Now()
						wctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
						o = modelSpan(wctx, tracer, "provider")
						if attach == "late" || attach == "late-after-two" {
							provider.RegisterSpanProcessor(p2)
						}
						o.End()
						hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
					} else {
						var octx context.Context
						octx, o = tracer.Start(ctx, "provider", trace.WithAttributes(modelSpanAttr))
						wrapperIn(octx, t, i)
						if attach == "late" || attach == "late-after-two" {
							provider.RegisterSpanProcessor(p2)
						}
						o.End()
					}
					i.End(rudderstackai.EndOptions{})
					if err := provider.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					spans := spanEvents(events())
					id := o.SpanContext().SpanID().String()
					n := 0
					for _, e := range spans {
						if e["properties"].(object)["span_id"] == id {
							n++
						}
					}
					want := 1 // outer: the SDK span under the OTel span's ID
					if inner {
						want = 0 // covered: the SDK span has its own ID
					}
					if n != want || len(spans) != 1 || !isSDKSpan(spans[0]) {
						t.Fatalf("%d spans (%v), %d under the OTel span's ID, want the SDK span alone", len(spans), spanNames(spans), n)
					}
				})
			}
		}
	}
}
