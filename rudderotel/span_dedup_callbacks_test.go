package rudderotel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// noHang runs fn and fails, with every goroutine's stack, when it has not returned in time: app
// code that a dedup, coverage or delivery lock is held around deadlocks on re-entry.
func noHang(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		msg := fmt.Sprintf("%s hung: app code ran under a span dedup lock\n%s", t.Name(), buf[:runtime.Stack(buf, true)])
		// The lock stays held, so this test's cleanup would hang too: crash the binary from
		// another goroutine, which skips cleanups.
		go func() { panic(msg) }()
		select {}
	}
}

// reentry is what an app callback does on re-entry: end the outer span, start and end another
// OTel model span, or make another wrapped model call, in ctx; or, while an inner model span
// starts inside a call, end that call or that span.
type reentry string

const (
	endOuter    reentry = "end-outer"
	otelModel   reentry = "otel-model"
	wrapperCall reentry = "wrapper-call"
	endCall     reentry = "end-call"
	endInner    reentry = "end-inner"
)

// reentrySpans are the spans app code re-entering dedup can reach.
type reentrySpans struct {
	ctx    context.Context
	tracer trace.Tracer
	i      *rudderstackai.Interaction
	outer  trace.Span
	// call is the wrapper call's span; inner the model span starting inside it.
	call  any
	inner *startedSpans
}

func recordCall(i *rudderstackai.Interaction, s any, model string) {
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: model, InputTokens: 1, OutputTokens: 1}, s, 200)
}

func (r reentry) run(h reentrySpans) {
	switch r {
	case endOuter:
		h.outer.End()
	case otelModel:
		modelSpan(h.ctx, h.tracer, "side").End()
	case wrapperCall:
		_, s := hook.Tracing.BeginWrapper(h.ctx, h.i, time.Now())
		recordCall(h.i, s, "side")
	case endCall:
		recordCall(h.i, h.call, "w")
	case endInner:
		h.inner.get("inner").End()
	}
}

// startedSpans is a processor ahead of RudderStack's that keeps each span as it starts, so app
// code can end one before RudderStack's processor has seen it start.
type startedSpans struct{ spans sync.Map }

func (p *startedSpans) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) {
	p.spans.Store(s.Name(), s)
}
func (p *startedSpans) OnEnd(sdktrace.ReadOnlySpan)      {}
func (p *startedSpans) Shutdown(context.Context) error   { return nil }
func (p *startedSpans) ForceFlush(context.Context) error { return nil }
func (p *startedSpans) get(name string) trace.Span {
	s, _ := p.spans.Load(name)
	return s.(trace.Span)
}

// startedFirst is testProvider with first ahead of RudderStack's processor.
func startedFirst(t *testing.T, options Options, first sdktrace.SpanProcessor) (*sdktrace.TracerProvider, func() []object) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	r, events := recorder(t)
	options.Analytics = r
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r)); err != nil {
		t.Fatal(err)
	}
	p, err := NewSpanProcessor(options)
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(first), sdktrace.WithSpanProcessor(p), sdktrace.WithResource(resource.Empty()))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return provider, events
}

// spanByName is the one exported span named name, or nil when there is none; more than one fails.
func spanByName(t *testing.T, spans []object, name string) object {
	t.Helper()
	var found object
	for _, e := range spans {
		if p := e["properties"].(object); p["span_name"] == name {
			if found != nil {
				t.Errorf("%s exported twice: %v", name, spanNames(spans))
			}
			found = p
		}
	}
	return found
}

// The size check that admits an SDK span (adoption of an outer OTel model span, coverage of an
// inner one) maps it through the shared mapper, which can call app code: the redact hook, on
// content. A call's core carries no content today, so app code stands in for the hook here, at
// the check's attribute normalization. It ends the outer span, starts and ends another model
// span, or makes another wrapped call, re-entering dedup on the same goroutine; or, for coverage,
// ends the call or the inner span. No lock is held around it, and a span whose state it changed
// is neither adopted nor covered: it exports as without dedup, and no entry outlives it.
func TestSpanDedupAdmissionRunsAppCodeWithoutALock(t *testing.T) {
	for _, shape := range []string{"adoption", "coverage"} {
		reentries := []reentry{endOuter, otelModel, wrapperCall}
		if shape == "coverage" {
			reentries = append(reentries, endCall, endInner)
		}
		for _, r := range reentries {
			for _, simple := range []bool{true, false} {
				t.Run(strings.Join([]string{shape, string(r), "simple=" + strconvBool(simple)}, "/"), func(t *testing.T) {
					var armed atomic.Bool
					var during func()
					normalize := hook.Tracing.NormalizeValue
					// Restored after the provider shuts down: cleanups run last in, first out.
					t.Cleanup(func() { hook.Tracing.NormalizeValue = normalize })
					hook.Tracing.NormalizeValue = func(v any) (any, bool) {
						if armed.CompareAndSwap(true, false) {
							during()
						}
						return normalize(v)
					}
					started := &startedSpans{}
					provider, events := startedFirst(t, Options{Simple: simple}, started)
					configureSpans(t)
					tracer := provider.Tracer("app")
					var outer trace.Span
					noHang(t, func() {
						ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
						if shape == "adoption" {
							// The wrapper call's admission, under the outer model span's IDs.
							ctx, outer = tracer.Start(ctx, "outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
							during = func() { r.run(reentrySpans{ctx: ctx, tracer: tracer, i: i, outer: outer}) }
							armed.Store(true)
							_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
							armed.Store(false)
							recordCall(i, s, "w")
						} else {
							// The wrapper call's admission when the inner model span starts inside it.
							ctx, outer = tracer.Start(ctx, "outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "invoke_agent")))
							wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
							during = func() {
								r.run(reentrySpans{ctx: wctx, tracer: tracer, i: i, outer: outer, call: s, inner: started})
							}
							armed.Store(true)
							inner := modelSpan(wctx, tracer, "inner")
							armed.Store(false)
							inner.End()
							if r != endCall {
								recordCall(i, s, "w")
							}
						}
						outer.End()
						i.End(rudderstackai.EndOptions{})
						if err := provider.ForceFlush(context.Background()); err != nil {
							t.Error(err)
						}
					})
					if armed.Load() {
						t.Fatal("the size check never ran the app code")
					}
					if n := liveRegistrations(); n != 0 {
						t.Errorf("%d dedup entries outlived their spans", n)
					}
					spans := spanEvents(events())
					requireIntactTrees(t, spans)
					ids := outer.SpanContext()
					call := spanByName(t, spans, "chat w")
					if call == nil {
						t.Fatalf("the wrapper call's span was not sent: %v", spanNames(spans))
					}
					switch {
					case r == endCall || r == endInner:
						// The call or the inner span ended during the check: nothing is covered.
						if spanByName(t, spans, "inner") == nil || spanByName(t, spans, "outer") == nil {
							t.Errorf("want the inner span exported as without dedup: %v", spanNames(spans))
						}
					case shape == "coverage":
						// The inner span is covered; the outer one is not a model span.
						if spanByName(t, spans, "inner") != nil || spanByName(t, spans, "outer") == nil {
							t.Errorf("want the outer span and the SDK span, not the inner one: %v", spanNames(spans))
						}
						if r == otelModel && spanByName(t, spans, "side") != nil {
							t.Errorf("a model span inside the call was not covered: %v", spanNames(spans))
						}
					case r == endOuter:
						// The outer span ended during the check: it exports as without dedup, and
						// the call is its child.
						if spanByName(t, spans, "outer") == nil || call["span_id"] == ids.SpanID().String() || call["parent_span_id"] != ids.SpanID().String() {
							t.Errorf("want the outer span exported with the call under it: %v, call %v/%v", spanNames(spans), call["span_id"], call["parent_span_id"])
						}
					case r == wrapperCall:
						// The other call adopted the outer span first; this one is its child.
						side := spanByName(t, spans, "chat side")
						if spanByName(t, spans, "outer") != nil || side == nil || side["span_id"] != ids.SpanID().String() || call["parent_span_id"] != ids.SpanID().String() {
							t.Errorf("want the other call under the outer span's IDs and this one its child: %v", spanNames(spans))
						}
					default:
						// Nothing changed the outer span: the call takes its place.
						if spanByName(t, spans, "outer") != nil || call["span_id"] != ids.SpanID().String() || spanByName(t, spans, "side") == nil {
							t.Errorf("want the call under the outer span's IDs and the other model span exported: %v", spanNames(spans))
						}
					}
				})
			}
		}
	}
}

// The app's callbacks (redact hook, Filter, ResolveIdentity, ExtraPropertiesFrom) run with no
// dedup, coverage or delivery lock of the SDK held, so one that ends the outer span, starts and
// ends another model span inside the call, or makes another wrapped call does not hang. The tree:
// an outer model span the call adopts, an inner model span it covers, and a tool span whose export
// runs the callbacks. (With Simple, OTel's simple processor exports under its own lock, so a
// callback ending a span that is then exported deadlocks there, with or without dedup.)
func TestSpanDedupAppCallbacksReenterWithoutHanging(t *testing.T) {
	for _, callback := range []string{"redact", "filter", "identity", "extra"} {
		for _, r := range []reentry{endOuter, otelModel, wrapperCall} {
			for _, simple := range []bool{true, false} {
				t.Run(strings.Join([]string{callback, string(r), "simple=" + strconvBool(simple)}, "/"), func(t *testing.T) {
					var armed atomic.Bool
					var during atomic.Pointer[func()]
					fire := func() {
						if armed.CompareAndSwap(true, false) {
							(*during.Load())()
						}
					}
					options := Options{Simple: simple}
					switch callback {
					case "filter":
						options.Filter = func(s sdktrace.ReadOnlySpan) bool { fire(); return IsAISpan(s) }
					case "identity":
						options.ResolveIdentity = func(sdktrace.ReadOnlySpan) Identity { fire(); return Identity{} }
					case "extra":
						options.ExtraPropertiesFrom = func(sdktrace.ReadOnlySpan) map[string]any { fire(); return nil }
					}
					provider, _, events := testProvider(t, options)
					configureSpans(t, rudderstackai.WithCaptureContent(true), rudderstackai.WithRedactHook(func(text string) (string, error) {
						if callback == "redact" {
							fire()
						}
						return text, nil
					}))
					tracer := provider.Tracer("app")
					var outer trace.Span
					noHang(t, func() {
						ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
						ctx, outer = tracer.Start(ctx, "outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
						wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
						run := func() { r.run(reentrySpans{ctx: wctx, tracer: tracer, i: i, outer: outer}) }
						during.Store(&run)
						ictx, inner := tracer.Start(wctx, "inner", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
						armed.Store(true)
						_, tool := tracer.Start(ictx, "tool", trace.WithAttributes(attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("gen_ai.tool.call.arguments", `{"q":"x"}`)))
						tool.End()
						inner.End()
						recordCall(i, s, "w")
						outer.End()
						i.End(rudderstackai.EndOptions{})
						if err := provider.ForceFlush(context.Background()); err != nil {
							t.Error(err)
						}
					})
					if armed.Load() {
						t.Fatal("the callback never ran")
					}
					spans := spanEvents(events())
					call := spanByName(t, spans, "chat w")
					if call == nil || call["span_id"] != outer.SpanContext().SpanID().String() {
						t.Fatalf("want the call's span under the outer span's IDs: %v", spanNames(spans))
					}
					if spanByName(t, spans, "outer") != nil || spanByName(t, spans, "inner") != nil || spanByName(t, spans, "tool") == nil {
						t.Errorf("want the outer and inner spans replaced and the tool span exported: %v", spanNames(spans))
					}
					spanByName(t, spans, "side")
					spanByName(t, spans, "chat side")
				})
			}
		}
	}
}
