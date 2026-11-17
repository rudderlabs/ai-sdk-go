package rudderotel

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// pastDefaultLimit is more spans than the default MaxMapEntries.
const pastDefaultLimit = 10_050

// delayedAPM is an APM exporter, behind OTel's simple processor, whose export of the span named
// target waits for release: every processor after it waits too.
type delayedAPM struct{ entered, release chan struct{} }

func (e *delayedAPM) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) > 0 && spans[0].Name() == "target" {
		close(e.entered)
		<-e.release
	}
	return nil
}

func (*delayedAPM) Shutdown(context.Context) error { return nil }

// endSignal reports each span's end, as the processors before it have seen it.
type endSignal chan string

func (endSignal) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (c endSignal) OnEnd(s sdktrace.ReadOnlySpan)                 { c <- s.Name() }
func (endSignal) Shutdown(context.Context) error                  { return nil }
func (endSignal) ForceFlush(context.Context) error                { return nil }

func waitFor[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// otherInteractionCalls ends n covered model spans of another interaction's call, through their
// own provider and processor.
func otherInteractionCalls(t *testing.T, n int, options Options) {
	t.Helper()
	p, err := NewSpanProcessor(options)
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "other"})
	wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	for range n {
		modelSpan(wctx, provider.Tracer("other"), "other").End()
	}
	recordCall(i, s, "other")
	i.End(rudderstackai.EndOptions{})
}

// spansUnder counts the exported spans under span ID id: the SDK's and the original OTel span's.
func spansUnder(spans []object, id string) (sdk, original int) {
	for _, e := range spans {
		if e["properties"].(object)["span_id"] != id {
			continue
		}
		if isSDKSpan(e) {
			sdk++
		} else {
			original++
		}
	}
	return sdk, original
}

// A RudderStack processor that ends a replaced or covered span after a slow one (here a real
// simple APM exporter) still drops it, while other calls end meanwhile: the span's own calls on
// other goroutines with MaxMapEntries 1, or more than the default MaxMapEntries covered spans of
// another interaction. That holds whether the delayed processor saw the span start, was
// registered after it started, or only forwards ends; and with Go collecting in between.
func TestSpanDedupDelayedProcessorStillDropsTheSpan(t *testing.T) {
	for _, attach := range []string{"saw-start", "late", "end-only"} {
		for _, churn := range []string{"own-calls-limit-1", "other-interaction-default-limit"} {
			for _, inner := range []bool{false, true} {
				for _, simple := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/inner=%v/simple=%v", attach, churn, inner, simple), func(t *testing.T) {
						_, p1, events := testProvider(t, Options{Simple: simple})
						if churn == "own-calls-limit-1" {
							configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1}))
						} else {
							configureSpans(t)
						}
						client := p1.exporter.client
						p2, err := NewSpanProcessor(Options{Simple: simple, Analytics: client})
						if err != nil {
							t.Fatal(err)
						}
						apm := &delayedAPM{entered: make(chan struct{}), release: make(chan struct{})}
						var once sync.Once
						release := func() { once.Do(func() { close(apm.release) }) }
						reached := endSignal(make(chan string, 16))
						processors := []sdktrace.SpanProcessor{p1, reached, sdktrace.NewSimpleSpanProcessor(apm)}
						switch attach {
						case "saw-start":
							processors = append(processors, p2)
						case "end-only":
							processors = append(processors, endOnly{p2})
						}
						var opts []sdktrace.TracerProviderOption
						for _, p := range processors {
							opts = append(opts, sdktrace.WithSpanProcessor(p))
						}
						provider := sdktrace.NewTracerProvider(opts...)
						t.Cleanup(func() {
							release()
							_ = provider.Shutdown(context.Background())
							_ = p2.Shutdown(context.Background())
						})
						tracer := provider.Tracer("app")
						ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
						var target trace.Span
						if inner {
							wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
							target = modelSpan(wctx, tracer, "target")
							recordCall(i, s, "m")
						} else {
							var tctx context.Context
							tctx, target = tracer.Start(ctx, "target", trace.WithAttributes(modelSpanAttr))
							wrapperIn(tctx, t, i)
						}
						if attach == "late" {
							provider.RegisterSpanProcessor(p2)
						}
						id := target.SpanContext().SpanID().String()
						var wg sync.WaitGroup
						end := target.End
						target = nil
						wg.Go(func() { end() })
						// p1 has ended the span; p2 waits behind the APM export.
						waitFor(t, apm.entered, "the APM export")
						waitFor(t, reached, "the target's end")
						if churn == "own-calls-limit-1" {
							for j := range 2 {
								c, o := tracer.Start(ctx, fmt.Sprint("call", j), trace.WithAttributes(modelSpanAttr))
								wrapperIn(c, t, i)
								wg.Go(func() { o.End() })
								waitFor(t, reached, "another call's end")
							}
						} else {
							otherInteractionCalls(t, pastDefaultLimit, Options{Simple: true, Analytics: client})
						}
						runtime.GC()
						release()
						wg.Wait()
						i.End(rudderstackai.EndOptions{})
						if err := provider.ForceFlush(context.Background()); err != nil {
							t.Fatal(err)
						}
						sdk, original := spansUnder(spanEvents(events()), id)
						wantSDK := 1 // outer: the SDK span under the OTel span's ID
						if inner {
							wantSDK = 0 // covered: the SDK span has its own ID
						}
						if sdk != wantSDK || original != 0 {
							t.Errorf("under the target's ID: %d SDK and %d original OTel spans, want %d and 0", sdk, original, wantSDK)
						}
					})
				}
			}
		}
	}
}

// Descendants started from a saved context after their covered parent ended (a tool under the
// model, a retrieval under the tool, an agent under the retrieval, each after its parent ended)
// join the tree under the SDK owner, whatever ends in between: more covered spans of the same
// call or of another interaction, past MaxMapEntries 1 or the default limit, and Go collecting.
func TestSpanDedupLateDescendantsKeepTheTree(t *testing.T) {
	for _, limit := range []int{1, 0} {
		for _, churn := range []string{"same-call", "other-interaction"} {
			for _, simple := range []bool{false, true} {
				name := "default"
				if limit != 0 {
					name = fmt.Sprint(limit)
				}
				t.Run(fmt.Sprintf("limit=%s/%s/simple=%v", name, churn, simple), func(t *testing.T) {
					provider, p, events := testProvider(t, blockingBatch(simple))
					n := pastDefaultLimit
					if limit != 0 {
						configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: limit}))
						n = limit + 1
					} else {
						configureSpans(t)
					}
					tracer := provider.Tracer("app")
					ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
					started := time.Now()
					wctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
					others := func() {
						if churn == "same-call" {
							for range n {
								modelSpan(wctx, tracer, "churn").End()
							}
						} else {
							otherInteractionCalls(t, n, Options{Simple: true, Analytics: p.exporter.client})
						}
						runtime.GC()
					}
					late := func(parent context.Context, name, op string) context.Context {
						c, d := tracer.Start(parent, name, trace.WithAttributes(attribute.String("gen_ai.operation.name", op)))
						d.End()
						others()
						return c
					}
					mctx, m := tracer.Start(wctx, "model", trace.WithAttributes(modelSpanAttr))
					m.End()
					others()
					rctx := late(late(mctx, "late-tool", "execute_tool"), "late-retrieval", "retrieval")
					late(rctx, "late-agent", "invoke_agent")
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
					i.End(rudderstackai.EndOptions{})
					if err := provider.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					spans := spanEvents(events())
					requireIntactTrees(t, spans)
					if names := spanNames(spans); countName(names, "model") != 0 {
						t.Errorf("the covered model was exported: %v", names)
					}
					call := spanByName(t, spans, "chat m")
					if call == nil {
						t.Fatalf("no SDK span: %v", spanNames(spans))
					}
					parent := call
					for _, name := range []string{"late-tool", "late-retrieval", "late-agent"} {
						d := spanByName(t, spans, name)
						if d == nil {
							t.Fatalf("%s not exported: %v", name, spanNames(spans))
						}
						if d["trace_id"] != call["trace_id"] || d["parent_span_id"] != parent["span_id"] {
							t.Errorf("%s on trace %v under %v, want trace %v under %v", name, d["trace_id"], d["parent_span_id"], call["trace_id"], parent["span_id"])
						}
						parent = d
					}
				})
			}
		}
	}
}

// When every processor that saw a replaced or covered span start shuts down while it is open,
// one that only forwards ends still drops it at its end, after other spans churn and Go collects.
func TestSpanDedupShutdownKeepsTheMarkForLaterEnds(t *testing.T) {
	for _, inner := range []bool{false, true} {
		t.Run("inner="+strconvBool(inner), func(t *testing.T) {
			_, p1, events := testProvider(t, Options{Simple: true})
			configureSpans(t)
			p2, err := NewSpanProcessor(Options{Simple: true, Analytics: p1.exporter.client})
			if err != nil {
				t.Fatal(err)
			}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(endOnly{p2}))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			var target trace.Span
			if inner {
				wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
				target = modelSpan(wctx, provider.Tracer("app"), "target")
				recordCall(i, s, "m")
			} else {
				var tctx context.Context
				tctx, target = provider.Tracer("app").Start(ctx, "target", trace.WithAttributes(modelSpanAttr))
				wrapperIn(tctx, t, i)
			}
			if err := p1.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			otherInteractionCalls(t, pastDefaultLimit, Options{Simple: true, Analytics: p1.exporter.client})
			runtime.GC()
			target.End()
			i.End(rudderstackai.EndOptions{})
			sdk, original := spansUnder(spanEvents(events()), target.SpanContext().SpanID().String())
			wantSDK := 1
			if inner {
				wantSDK = 0
			}
			if sdk != wantSDK || original != 0 {
				t.Errorf("under the target's ID: %d SDK and %d original OTel spans, want %d and 0", sdk, original, wantSDK)
			}
		})
	}
}

// A closed entry stays while the app can still reach its span, whatever Go collects, and goes
// once Go collects the span: it is never evicted, and the dedup state does not keep spans.
func TestSpanDedupClosedEntriesGoWithTheirSpans(t *testing.T) {
	provider, p, _ := testProvider(t, Options{Simple: true})
	configureSpans(t)
	keys := func() []spanKey {
		tracer := provider.Tracer("app")
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
		wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
		mctx, m := tracer.Start(wctx, "model", trace.WithAttributes(modelSpanAttr))
		_, tool := tracer.Start(mctx, "tool", trace.WithAttributes(attribute.String("gen_ai.operation.name", "execute_tool")))
		tool.End()
		m.End()
		recordCall(i, s, "m")
		i.End(rudderstackai.EndOptions{})
		keys := []spanKey{key(m.SpanContext()), key(tool.SpanContext())}
		otherInteractionCalls(t, pastDefaultLimit, Options{Simple: true, Analytics: p.exporter.client})
		runtime.GC()
		runtime.GC()
		for _, k := range keys {
			if !closedEntry(k) {
				t.Fatal("a closed entry went while its span was reachable")
			}
		}
		runtime.KeepAlive(mctx)
		runtime.KeepAlive(tool)
		return keys
	}()
	deadline := time.Now().Add(10 * time.Second)
	for closedEntry(keys[0]) || closedEntry(keys[1]) {
		if time.Now().After(deadline) {
			t.Fatal("closed entries outlived their spans")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}
