package rudderotel

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// requireIntactTrees asserts that every exported span's parent was exported too, on the same
// trace: RudderStack never gets a span under a parent it never received.
func requireIntactTrees(t *testing.T, spans []object) {
	t.Helper()
	sent := map[[2]string]bool{}
	for _, e := range spans {
		p := e["properties"].(object)
		sent[[2]string{p["trace_id"].(string), p["span_id"].(string)}] = true
	}
	for _, e := range spans {
		p := e["properties"].(object)
		if parent, ok := p["parent_span_id"].(string); ok && !sent[[2]string{p["trace_id"].(string), parent}] {
			t.Errorf("orphan %v: parent %s was not exported on trace %v", p["span_name"], parent, p["trace_id"])
		}
	}
}

func blockingBatch(simple bool) Options {
	// A blocking queue, so churn never drops the spans under test.
	return Options{Simple: simple, BatchOptions: []sdktrace.BatchSpanProcessorOption{sdktrace.WithBlocking()}}
}

// coveredTreesRun runs one wrapper call with two instrumented model spans inside, each with a
// tool child, a retrieval under the tool and an agent under the retrieval. The models end first,
// then during runs, then the descendants end (deepest first), then the call. It returns the
// exported spans.
func coveredTreesRun(t *testing.T, provider *sdktrace.TracerProvider, events func() []object, during func()) []object {
	t.Helper()
	tracer := provider.Tracer("app")
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	started := time.Now()
	wctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
	var models, descendants []trace.Span
	for _, name := range []string{"first", "second"} {
		ctx, m := tracer.Start(wctx, name+"-model", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
		models = append(models, m)
		for _, op := range []string{"execute_tool", "retrieval", "invoke_agent"} {
			var d trace.Span
			ctx, d = tracer.Start(ctx, name+"-"+op, trace.WithAttributes(attribute.String("gen_ai.operation.name", op)))
			descendants = append([]trace.Span{d}, descendants...)
		}
	}
	for _, m := range models {
		m.End()
	}
	if during != nil {
		during()
	}
	for _, d := range descendants {
		d.End()
	}
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
	i.End(rudderstackai.EndOptions{})
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := spanEvents(events())
	requireIntactTrees(t, spans)
	// Every descendant is exported; each model is exported or covered by the SDK span.
	names := spanNames(spans)
	for _, d := range []string{"execute_tool", "retrieval", "invoke_agent"} {
		for _, name := range []string{"first", "second"} {
			if countName(names, name+"-"+d) != 1 {
				t.Errorf("%s-%s exported %d times: %v", name, d, countName(names, name+"-"+d), names)
			}
		}
	}
	if countName(names, "chat m") != 1 {
		t.Errorf("SDK span exported %d times: %v", countName(names, "chat m"), names)
	}
	return spans
}

// churn starts and ends n parent/child pairs of non-AI spans through provider: other requests'
// work, overlapping the tree under test.
func churn(provider *sdktrace.TracerProvider, n int) {
	tracer := provider.Tracer("other")
	for range n {
		ctx, request := tracer.Start(context.Background(), "request", trace.WithAttributes(attribute.String("http.request.method", "GET")))
		_, db := tracer.Start(ctx, "db", trace.WithAttributes(attribute.String("db.system", "postgresql")))
		db.End()
		request.End()
	}
}

// With MaxMapEntries 1, a covered model's live descendants keep their place: a model that cannot
// be tracked is not covered and exports as without dedup, so no exported span loses its parent.
func TestSpanDedupLimitOneKeepsEveryTreeIntact(t *testing.T) {
	for _, simple := range []bool{true, false} {
		t.Run("simple="+strconvBool(simple), func(t *testing.T) {
			provider, _, events := testProvider(t, blockingBatch(simple))
			configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1}))
			coveredTreesRun(t, provider, events, nil)
		})
	}
}

// At the default limit, other requests' work never evicts what a covered model's open
// descendants need, whether it runs between their starts and ends or alongside them.
func TestSpanDedupDefaultLimitUnderOverlappingWork(t *testing.T) {
	const others = 10_050 // more than the default MaxMapEntries
	for _, simple := range []bool{true, false} {
		t.Run("sequential/simple="+strconvBool(simple), func(t *testing.T) {
			provider, _, events := testProvider(t, blockingBatch(simple))
			configureSpans(t)
			spans := coveredTreesRun(t, provider, events, func() { churn(provider, others) })
			// With room, both models are covered: only the SDK span stands for them.
			if names := spanNames(spans); countName(names, "first-model")+countName(names, "second-model") != 0 {
				t.Errorf("covered models exported: %v", names)
			}
		})
		t.Run("concurrent/simple="+strconvBool(simple), func(t *testing.T) {
			provider, _, events := testProvider(t, blockingBatch(simple))
			configureSpans(t)
			tracer := provider.Tracer("app")
			var wg sync.WaitGroup
			wg.Go(func() { churn(provider, others) })
			for g := range 8 {
				wg.Go(func() {
					for r := range 25 {
						ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: fmt.Sprint("u", g)})
						started := time.Now()
						wctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
						mctx, m := tracer.Start(wctx, "model", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
						tctx, tool := tracer.Start(mctx, "tool", trace.WithAttributes(attribute.String("gen_ai.operation.name", "execute_tool")))
						_, r2 := tracer.Start(tctx, "retrieval", trace.WithAttributes(attribute.String("gen_ai.operation.name", "retrieval")))
						m.End()
						churn(provider, r)
						r2.End()
						tool.End()
						hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
						// Outer adoption alongside: a call inside an open model span.
						octx, o := tracer.Start(ctx, "outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
						_, os := hook.Tracing.BeginWrapper(octx, i, time.Now())
						hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "o"}, os, 200)
						o.End()
						i.End(rudderstackai.EndOptions{})
					}
				})
			}
			wg.Wait()
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			spans := spanEvents(events())
			requireIntactTrees(t, spans)
			names := spanNames(spans)
			if countName(names, "tool") != 200 || countName(names, "retrieval") != 200 {
				t.Errorf("tools %d, retrievals %d, want 200 each", countName(names, "tool"), countName(names, "retrieval"))
			}
			ids := map[string]int{}
			for _, e := range spans {
				ids[e["properties"].(object)["span_id"].(string)]++
			}
			for id, n := range ids {
				if n != 1 {
					t.Errorf("span ID %s exported %d times", id, n)
				}
			}
		})
	}
}

// A full registry refuses new coverage at the default limit too: the new model and its
// descendants export as without dedup, and coverage resumes once spans end.
func TestSpanDedupFullRegistryRefusesNewCoverage(t *testing.T) {
	for _, simple := range []bool{true, false} {
		t.Run("simple="+strconvBool(simple), func(t *testing.T) {
			options := blockingBatch(simple)
			// The spans that fill the registry are not the ones under test: not exported.
			options.Filter = func(s sdktrace.ReadOnlySpan) bool { return s.Name() != "held" && IsAISpan(s) }
			provider, _, events := testProvider(t, options)
			configureSpans(t)
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "held"})
			held := make([]trace.Span, 0, 10_000)
			for range 10_000 {
				_, o := provider.Tracer("app").Start(ctx, "held", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
				held = append(held, o)
			}
			spans := coveredTreesRun(t, provider, events, nil)
			if names := spanNames(spans); countName(names, "first-model") != 1 || countName(names, "second-model") != 1 {
				t.Errorf("with the registry full, models were covered: %v", names)
			}
			for _, o := range held {
				o.End()
			}
			i.End(rudderstackai.EndOptions{})
			if n := liveRegistrations(); n != 0 {
				t.Fatalf("%d entries left after every span ended", n)
			}
		})
	}
}

// The registry of open model spans keeps at most MaxMapEntries: a span starting while it is
// full is not registered, so a call inside it keeps its own span and the OTel span exports as
// without dedup. Registered spans are never evicted, and an entry goes when its span ends.
func TestSpanDedupRegistryIsBoundedByMaxMapEntries(t *testing.T) {
	for _, simple := range []bool{true, false} {
		t.Run("simple="+strconvBool(simple), func(t *testing.T) {
			provider, _, events := testProvider(t, blockingBatch(simple))
			var mu sync.Mutex
			var reported []rudderstackai.ErrorInfo
			configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}), rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
				mu.Lock()
				defer mu.Unlock()
				reported = append(reported, info)
			}))
			tracer := provider.Tracer("app")
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			start := func(name string) (context.Context, trace.Span) {
				return tracer.Start(ctx, name, trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
			}
			var heldCtx []context.Context
			var held []trace.Span
			for range 2 {
				c, o := start("held")
				heldCtx, held = append(heldCtx, c), append(held, o)
			}
			c, extra := start("extra")
			if id := wrapperIn(c, t, i); id == extra.SpanContext().SpanID().String() {
				t.Error("a span started with the registry full was adopted")
			}
			extra.End()
			mu.Lock()
			if want := (rudderstackai.ErrorInfo{Kind: rudderstackai.KindLimitReached, Where: "MaxMapEntries (span dedup)"}); !slices.Contains(reported, want) {
				t.Errorf("reported %v, want %v", reported, want)
			}
			mu.Unlock()
			for k, c := range heldCtx {
				if id := wrapperIn(c, t, i); id != held[k].SpanContext().SpanID().String() {
					t.Errorf("held span %d was not adopted: a live entry was evicted", k)
				}
				held[k].End()
			}
			c, again := start("again")
			if id := wrapperIn(c, t, i); id != again.SpanContext().SpanID().String() {
				t.Error("an ended span's entry was not released: a new span was not adopted")
			}
			again.End()
			i.End(rudderstackai.EndOptions{})
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			spans := spanEvents(events())
			requireIntactTrees(t, spans)
			ids := map[string]int{}
			for _, e := range spans {
				ids[e["properties"].(object)["span_id"].(string)]++
			}
			// held x2 and again: their SDK spans; extra: the OTel span and the call's own span.
			if len(spans) != 5 || len(ids) != 5 || ids[extra.SpanContext().SpanID().String()] != 1 {
				t.Errorf("got %d spans with IDs %v, want 5 distinct including the OTel span extra", len(spans), ids)
			}
		})
	}
}

// Abandoned model spans never hold more than MaxMapEntries entries, and Shutdown releases them.
func TestSpanDedupAbandonedSpansStayWithinTheLimit(t *testing.T) {
	provider, p, _ := testProvider(t, Options{Simple: true})
	configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1}))
	before := liveRegistrations()
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	for range 10 {
		_, _ = provider.Tracer("app").Start(ctx, "abandoned", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	}
	wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	for range 10 {
		_ = modelSpan(wctx, provider.Tracer("app"), "abandoned inner")
	}
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m"}, s, 200)
	i.End(rudderstackai.EndOptions{})
	if n := liveRegistrations() - before; n > 1 {
		t.Errorf("%d abandoned spans held, limit 1", n)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := liveRegistrations(); n != 0 {
		t.Errorf("%d entries left after Shutdown", n)
	}
}
