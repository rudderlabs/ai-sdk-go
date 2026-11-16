package rudderotel

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type dedupNode struct {
	Kind              string      `json:"kind"`
	Label             string      `json:"label"`
	Operation         string      `json:"operation"`
	EndBeforeChildren bool        `json:"end_before_children"`
	Legacy            bool        `json:"legacy"`
	Children          []dedupNode `json:"children"`
}

type dedupScenario struct {
	Name          string    `json:"name"`
	EmitSpans     bool      `json:"emit_spans"`
	Requires      string    `json:"requires"`
	Tree          dedupNode `json:"tree"`
	ExpectedSpans []struct {
		Label  string  `json:"label"`
		Parent *string `json:"parent"`
	} `json:"expected_spans"`
	CallSpan           *string  `json:"call_span"`
	OtherProcessorsSee []string `json:"other_processors_see"`
}

// dedupRun drives one scenario tree: interactions, wrapper calls (children run inside the
// provider request, as an instrumentation would), manual spans and active OTel spans.
func dedupRun(ctx context.Context, tracer trace.Tracer, n dedupNode) {
	children := func(ctx context.Context) {
		for _, c := range n.Children {
			dedupRun(ctx, tracer, c)
		}
	}
	switch n.Kind {
	case "interaction":
		ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: n.Label})
		children(ctx)
		i.End(rudderstackai.EndOptions{})
	case "manual":
		ctx, s := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: n.Label, Operation: n.Operation})
		children(ctx)
		s.End(nil)
	case "otel":
		attrs := []attribute.KeyValue{attribute.String("gen_ai.operation.name", n.Operation), attribute.String("gen_ai.system", "openai")}
		if n.Legacy {
			attrs = []attribute.KeyValue{attribute.String("llm.request.type", n.Operation)}
		}
		ctx, s := tracer.Start(ctx, n.Label, trace.WithAttributes(attrs...))
		if n.EndBeforeChildren {
			s.End()
		}
		children(ctx)
		s.End()
	case "wrapper":
		i := rudderstackai.FromContext(ctx)
		started := time.Now()
		ctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
		children(ctx)
		hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: n.Label, ResponseID: "r-" + n.Label, InputTokens: 1, OutputTokens: 1, StartedAt: started, EndedAt: time.Now()}, s, 200)
	}
}

func TestSharedSpanDedupFixture(t *testing.T) {
	var fixture struct {
		Scenarios []dedupScenario `json:"scenarios"`
	}
	loadCases(t, "../spec/fixtures/span-dedup-v0.1.json", &fixture)
	for _, sc := range fixture.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Requires == "integration" {
				t.Skip("platform limit: Go has no framework integrations")
			}
			apm := tracetest.NewSpanRecorder()
			provider, _, events := testProvider(t, Options{Simple: true}, apm)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(sc.EmitSpans), rudderstackai.WithLLMCallEvents(true)); err != nil {
				t.Fatal(err)
			}
			dedupRun(context.Background(), provider.Tracer("app"), sc.Tree)

			labels := map[string]string{}
			seen := []string{}
			for _, s := range apm.Ended() {
				labels[s.SpanContext().SpanID().String()] = s.Name()
				seen = append(seen, s.Name())
			}
			exported := spanEvents(events())
			label := func(name any) string { return strings.TrimPrefix(name.(string), "chat ") }
			for _, e := range exported {
				p := e["properties"].(object)
				labels[p["span_id"].(string)] = label(p["span_name"])
			}
			if len(exported) != len(sc.ExpectedSpans) {
				t.Fatalf("exported %d spans, want %d: %v", len(exported), len(sc.ExpectedSpans), exported)
			}
			for _, want := range sc.ExpectedSpans {
				found := false
				for _, e := range exported {
					p := e["properties"].(object)
					if label(p["span_name"]) != want.Label {
						continue
					}
					found = true
					parent, _ := p["parent_span_id"].(string)
					switch {
					case want.Parent == nil && parent != "":
						t.Errorf("%s: parent %q, want none", want.Label, labels[parent])
					case want.Parent != nil && labels[parent] != *want.Parent:
						t.Errorf("%s: parent %q, want %q", want.Label, labels[parent], *want.Parent)
					}
				}
				if !found {
					t.Errorf("span %s not exported", want.Label)
				}
			}
			slices.Sort(seen)
			others := slices.Clone(sc.OtherProcessorsSee)
			slices.Sort(others)
			if !slices.Equal(seen, others) {
				t.Errorf("other processors saw %v, want %v", seen, others)
			}
			for _, e := range events() {
				if e["event"] != "ai_llm_response_received" {
					continue
				}
				id, _ := e["properties"].(object)["span_id"].(string)
				got := labels[id]
				if id == "" {
					got = ""
				}
				want := ""
				if sc.CallSpan != nil {
					want = *sc.CallSpan
				}
				if got != want {
					t.Errorf("call span_id is %q, want %q", got, want)
				}
			}
		})
	}
}

func TestSpanDedupNeverModifiesTheAppSpan(t *testing.T) {
	apm := tracetest.NewSpanRecorder()
	provider, _, events := testProvider(t, Options{Simple: true}, apm)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, app := provider.Tracer("app").Start(ctx, "chat gpt-4o-mini", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 5, OutputText: "hi", InputMessages: []rudderstackai.GenAIMessage{{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "hello"}}}}}, s, 200)
	app.End()
	i.End(rudderstackai.EndOptions{})
	spans := spanEvents(events())
	if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != app.SpanContext().SpanID().String() {
		t.Fatalf("want only the app span: %v", spans)
	}
	for _, kv := range apm.Ended()[0].Attributes() {
		if strings.HasPrefix(string(kv.Key), "gen_ai.usage.") || strings.HasPrefix(string(kv.Key), "gen_ai.input.") || kv.Key == "rudderstack.span_source" {
			t.Errorf("the SDK wrote %s onto the app span", kv.Key)
		}
	}
}

// recordOnly samples spans as recording but not sampled: the processor sees them, the exporter never does.
type recordOnly struct{}

func (recordOnly) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return sdktrace.SamplingResult{Decision: sdktrace.RecordOnly, Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState()}
}
func (recordOnly) Description() string { return "record-only" }

// wrapperIn runs one wrapper call in ctx and reports whether it sent a span of its own.
func wrapperIn(ctx context.Context, t *testing.T, i *rudderstackai.Interaction, events func() []object) bool {
	t.Helper()
	before := len(spanEvents(events()))
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
	return len(spanEvents(events())) > before
}

func TestSpanDedupOwnerMustBeExported(t *testing.T) {
	for _, c := range []struct {
		name    string
		options Options
		more    []sdktrace.TracerProviderOption
	}{
		{name: "filter rejects it", options: Options{Simple: true, Filter: func(sdktrace.ReadOnlySpan) bool { return false }}},
		{name: "record-only sampler", options: Options{Simple: true}, more: []sdktrace.TracerProviderOption{sdktrace.WithSampler(recordOnly{})}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, p, events := testProvider(t, c.options)
			provider := sdktrace.NewTracerProvider(append(c.more, sdktrace.WithSpanProcessor(p))...)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
			if !wrapperIn(ctx, t, i, events) {
				t.Error("a span the processor never exports owned the call")
			}
			o.End()
			i.End(rudderstackai.EndOptions{})
		})
	}
}

func TestSpanDedupEndedInteractionNeverJoins(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	defer o.End()
	i.End(rudderstackai.EndOptions{})
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	if span, _ := s.(*rudderstackai.Span); span == nil || span.SpanID == o.SpanContext().SpanID().String() {
		t.Fatalf("a wrapper of an ended interaction joined the OTel span: %v", s)
	}
	hook.Tracing.FinishRaw(s, "openai", "w", 200, time.Now(), nil)
	if len(spanEvents(events())) != 1 {
		t.Error("the wrapper sent no span of its own")
	}
}

func TestSpanDedupJoinedWrapperKeepsItsSDKParent(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, manual := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "manual", Operation: "execute_tool"})
	defer manual.End(nil)
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	retained, joined := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "first"}, joined, 200)
	o.End()
	// A later call on the retained context, after both the joined call and the OTel span ended.
	_, later := hook.Tracing.BeginWrapper(retained, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "later"}, later, 200)
	found := false
	for _, e := range spanEvents(events()) {
		p := e["properties"].(object)
		if p["span_name"] != "chat later" {
			continue
		}
		found = true
		if p["parent_span_id"] != manual.SpanID {
			t.Fatalf("later call parent %v, want the manual span %s", p["parent_span_id"], manual.SpanID)
		}
	}
	if !found {
		t.Fatal("the later call sent no span")
	}
}

func TestSpanDedupShutdownForgetsItsSpans(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	defer o.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !wrapperIn(ctx, t, i, events) {
		t.Error("a span of a shut-down processor owned the call")
	}
}

func TestSpanDedupRejectedOwnerSendsTheJoinedSpan(t *testing.T) {
	for _, order := range []string{"after", "before"} {
		t.Run("owner ends "+order+" the call", func(t *testing.T) {
			// Accepts the span at start, rejects it once it ends with ERROR.
			provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool { return IsAISpan(s) && s.Status().Code != codes.Error }})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true)); err != nil {
				t.Fatal(err)
			}
			tracer := provider.Tracer("app")
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			ctx, root := tracer.Start(ctx, "request")
			ctx, o := tracer.Start(ctx, "chat outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
			_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
			if order == "before" {
				o.SetStatus(codes.Error, "")
				o.End()
			}
			hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 1, OutputTokens: 1}, s, 200)
			if order == "after" {
				if n := len(spanEvents(events())); n != 0 {
					t.Fatalf("sent %d spans before the owner settled", n)
				}
				o.SetStatus(codes.Error, "")
				o.End()
			}
			root.End()
			i.End(rudderstackai.EndOptions{})
			spans := spanEvents(events())
			if len(spans) != 1 {
				t.Fatalf("want the joined span alone, got %d", len(spans))
			}
			p := spans[0]["properties"].(object)
			ids := o.SpanContext()
			if p["trace_id"] != ids.TraceID().String() || p["span_id"] != ids.SpanID().String() || p["parent_span_id"] != root.SpanContext().SpanID().String() {
				t.Errorf("fallback span IDs %v/%v/%v, want the owner's", p["trace_id"], p["span_id"], p["parent_span_id"])
			}
			if b, _ := json.Marshal(p["otlp"]); !strings.Contains(string(b), "rudderstack.span_source") {
				t.Error("the fallback is not the SDK's wrapper span")
			}
			for _, e := range events() {
				if e["event"] == "ai_llm_response_received" && e["properties"].(object)["span_id"] != ids.SpanID().String() {
					t.Errorf("call span_id %v, want the owner's", e["properties"].(object)["span_id"])
				}
			}
		})
	}
}

func TestSpanDedupStartFilterSeesIdentityTags(t *testing.T) {
	// Exports only spans tagged with the interaction's user.
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		return IsAISpan(s) && asString(attributes(s.Attributes())["user.id"]) == "u"
	}})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	if wrapperIn(ctx, t, i, events) {
		t.Error("the tagged OTel span did not own the call")
	}
	o.End()
	i.End(rudderstackai.EndOptions{})
	spans := spanEvents(events())
	if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
		t.Fatalf("want only the owner's span, got %d", len(spans))
	}
}

func TestSpanDedupPanickingEndFilterSendsTheJoinedSpan(t *testing.T) {
	// Accepts the span at start, panics once it has ended.
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		if !s.EndTime().IsZero() {
			panic("filter exploded")
		}
		return IsAISpan(s)
	}})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	if wrapperIn(ctx, t, i, events) {
		t.Fatal("the joined span was sent before its owner settled")
	}
	o.End()
	i.End(rudderstackai.EndOptions{})
	spans := spanEvents(events())
	if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
		t.Fatalf("want the held span under the owner's ID, got %d", len(spans))
	}
}

// ownerSpan starts an active OTel model-call span in a new interaction of user u.
func ownerSpan(tracer trace.Tracer) (context.Context, *rudderstackai.Interaction, trace.Span) {
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, o := tracer.Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	return ctx, i, o
}

func rejectErrors(s sdktrace.ReadOnlySpan) bool { return IsAISpan(s) && s.Status().Code != codes.Error }

func TestSpanDedupFlushFinishesAJoinedCall(t *testing.T) {
	for _, c := range []struct {
		method   string
		rejected bool
	}{{"Flush", true}, {"Flush", false}, {"Shutdown", true}} {
		rejected := c.rejected
		t.Run(c.method+"/owner rejected="+strconvBool(rejected), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, Filter: rejectErrors})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			hook.Tracing.BeginWrapper(ctx, i, time.Now()) // an unread joined call
			finish := rudderstackai.Flush
			if c.method == "Shutdown" {
				finish = rudderstackai.Shutdown
			}
			if !finish(context.Background()) {
				t.Fatalf("%s did not finish", c.method)
			}
			if rejected {
				o.SetStatus(codes.Error, "")
			}
			o.End()
			i.End(rudderstackai.EndOptions{})
			spans := spanEvents(events())
			if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
				t.Fatalf("want one span with the owner's ID, got %d", len(spans))
			}
			if src := strings.Contains(render(spans[0]), "rudderstack.span_source"); src != rejected {
				t.Errorf("SDK fallback sent=%v, want %v", src, rejected)
			}
		})
	}
}

func TestSpanDedupExporterUsesTheProcessorDecision(t *testing.T) {
	// Accepts the span at start and at the processor's end decision, rejects any later call.
	var calls atomic.Int32
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		return calls.Add(1) <= 2 && IsAISpan(s)
	}})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	wrapperIn(ctx, t, i, events)
	o.End()
	i.End(rudderstackai.EndOptions{})
	if spans := spanEvents(events()); len(spans) != 1 {
		t.Fatalf("the call lost its span: %d spans", len(spans))
	}
}

func TestSpanDedupProcessorsSettleTogether(t *testing.T) {
	accept := func(sdktrace.ReadOnlySpan) bool { return true }
	reject := func(s sdktrace.ReadOnlySpan) bool { return s.Status().Code != codes.Error }
	explode := func(sdktrace.ReadOnlySpan) bool { panic("filter exploded") }
	for _, c := range []struct {
		name     string
		ends     [2]func(sdktrace.ReadOnlySpan) bool
		count    int
		fallback bool
	}{
		{"one exports, one rejects", [2]func(sdktrace.ReadOnlySpan) bool{accept, reject}, 1, false},
		{"both reject", [2]func(sdktrace.ReadOnlySpan) bool{reject, reject}, 1, true},
		{"both export", [2]func(sdktrace.ReadOnlySpan) bool{accept, accept}, 2, false},
		{"one panics, one rejects", [2]func(sdktrace.ReadOnlySpan) bool{explode, reject}, 1, true},
		{"one exports, one panics", [2]func(sdktrace.ReadOnlySpan) bool{accept, explode}, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Accepted at start, so both processors register the owner; each decides at the end.
			filter := func(end func(sdktrace.ReadOnlySpan) bool) func(sdktrace.ReadOnlySpan) bool {
				return func(s sdktrace.ReadOnlySpan) bool { return s.EndTime().IsZero() || end(s) }
			}
			_, p1, events := testProvider(t, Options{Simple: true, Filter: filter(c.ends[0])})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			p2, err := NewSpanProcessor(Options{Simple: true, Filter: filter(c.ends[1])})
			if err != nil {
				t.Fatal(err)
			}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(p2))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			wrapperIn(ctx, t, i, events)
			o.SetStatus(codes.Error, "")
			o.End()
			i.End(rudderstackai.EndOptions{})
			spans := spanEvents(events())
			fallbacks := 0
			for _, s := range spans {
				if s["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
					t.Errorf("span %v is not the owner's", s["properties"].(object)["span_id"])
				}
				if strings.Contains(render(s), "rudderstack.span_source") {
					fallbacks++
				}
			}
			if len(spans) != c.count || (fallbacks > 0) != c.fallback {
				t.Fatalf("spans=%d fallbacks=%d, want %d spans and fallback=%v", len(spans), fallbacks, c.count, c.fallback)
			}
		})
	}
}

func TestSpanDedupExporterReusesARejection(t *testing.T) {
	// Accepts at start, rejects at the processor's end decision, accepts any later call.
	var calls atomic.Int32
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		return calls.Add(1) != 2 && IsAISpan(s)
	}})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	wrapperIn(ctx, t, i, events)
	o.End()
	i.End(rudderstackai.EndOptions{})
	spans := spanEvents(events())
	if len(spans) != 1 || !strings.Contains(render(spans[0]), "rudderstack.span_source") {
		t.Fatalf("want only the SDK span in the rejected owner's place, got %d", len(spans))
	}
}

func TestSpanDedupShutdownSendsTheJoinedSpanOfAnOpenOwner(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	defer i.End(rudderstackai.EndOptions{})
	wrapperIn(ctx, t, i, events)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := spanEvents(events())
	if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() || !strings.Contains(render(spans[0]), "rudderstack.span_source") {
		t.Fatalf("want the held SDK span under the open owner's ID, got %d", len(spans))
	}
}

func TestSpanDedupCapsReportLimitReached(t *testing.T) {
	for _, where := range []string{"span owners", "held spans"} {
		t.Run(where, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, Filter: rejectErrors})
			var mu sync.Mutex
			var reported []string
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1}),
				rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
					mu.Lock()
					defer mu.Unlock()
					if info.Kind == rudderstackai.KindLimitReached {
						reported = append(reported, info.Where)
					}
				})); err != nil {
				t.Fatal(err)
			}
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			if where == "span owners" {
				_, o2 := provider.Tracer("app").Start(ctx, "chat 2", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
				o2.End()
			} else {
				for range 2 {
					_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
					hook.Tracing.FinishRaw(s, "openai", "m", 200, time.Now(), nil)
				}
			}
			o.End()
			i.End(rudderstackai.EndOptions{})
			_ = events
			mu.Lock()
			defer mu.Unlock()
			if !slices.ContainsFunc(reported, func(w string) bool { return strings.Contains(w, where) }) {
				t.Errorf("no limit_reached for %s: %v", where, reported)
			}
		})
	}
}

func TestSpanDedupNoRegistrationAfterShutdown(t *testing.T) {
	_, p, _ := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A span whose OnStart passed the closed check just before Shutdown, registering now.
	source := sdktrace.NewTracerProvider()
	ctx, i, s := ownerSpan(source.Tracer("app"))
	defer i.End(rudderstackai.EndOptions{})
	defer s.End()
	p.dedup(ctx, s.(sdktrace.ReadWriteSpan), i)
	otelCalls.mu.Lock()
	_, registered := otelCalls.m[key(s.SpanContext())]
	otelCalls.mu.Unlock()
	if registered {
		t.Error("a shut-down processor registered an owner")
	}
}

// render renders an event for substring checks.
func render(e object) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func TestSpanDedupProcessorRejectingAtStartStillSettles(t *testing.T) {
	ok := func(s sdktrace.ReadOnlySpan) bool { return s.Status().Code != codes.Error }
	failed := func(s sdktrace.ReadOnlySpan) bool { return s.Status().Code == codes.Error }
	for _, reverse := range []bool{false, true} {
		t.Run("reverse="+strconvBool(reverse), func(t *testing.T) {
			// One accepts at start and rejects at the end; the other rejects at start and exports.
			filters := [2]func(sdktrace.ReadOnlySpan) bool{ok, failed}
			if reverse {
				filters[0], filters[1] = filters[1], filters[0]
			}
			provider, p, events := testProvider(t, Options{Simple: true, Filter: filters[0]})
			other, err := NewSpanProcessor(Options{Simple: true, Filter: filters[1], Analytics: p.exporter.client})
			if err != nil {
				t.Fatal(err)
			}
			provider.RegisterSpanProcessor(other)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			wrapperIn(ctx, t, i, events)
			o.SetStatus(codes.Error, "")
			o.End()
			i.End(rudderstackai.EndOptions{})
			spans := spanEvents(events())
			if len(spans) != 1 || spans[0]["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
				t.Fatalf("want only the exported owner, got %d spans", len(spans))
			}
			if strings.Contains(render(spans[0]), "rudderstack.span_source") {
				t.Error("the SDK fallback went out although a processor exported the owner")
			}
		})
	}
}

// blockedExporter holds the export of the span named busy until release closes.
type blockedExporter struct {
	sdktrace.SpanExporter
	entered, release chan struct{}
}

func (e *blockedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) > 0 && spans[0].Name() == "busy" {
		close(e.entered)
		<-e.release
	}
	return e.SpanExporter.ExportSpans(ctx, spans)
}

func TestSpanDedupFullBatchQueueKeepsTheOwnedCallsSpan(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true, ExportAllSpans: true})
	// A batch queue of one, its exporter stuck on busy and its queue holding queued.
	blocked := &blockedExporter{SpanExporter: p.exporter, entered: make(chan struct{}), release: make(chan struct{})}
	p.inner = sdktrace.NewBatchSpanProcessor(blocked, sdktrace.WithMaxQueueSize(1), sdktrace.WithMaxExportBatchSize(1), sdktrace.WithBatchTimeout(time.Minute))
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	tracer := provider.Tracer("app")
	_, busy := tracer.Start(context.Background(), "busy")
	busy.End()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the busy export did not start")
	}
	_, queued := tracer.Start(context.Background(), "queued")
	queued.End()
	ctx, i, o := ownerSpan(tracer)
	wrapperIn(ctx, t, i, events)
	o.End()
	i.End(rudderstackai.EndOptions{})
	close(blocked.release)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, e := range spanEvents(events()) {
		if n := e["properties"].(object)["span_name"]; n != "busy" && n != "queued" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("the owned call has %d spans, want 1", calls)
	}
}

func TestSpanDedupJoinedCallFixesTheSDKTraceAcrossPause(t *testing.T) {
	for _, restore := range []string{"in-process", "json"} {
		t.Run(restore, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			ownerTrace := o.SpanContext().TraceID().String()
			wrapperIn(ctx, t, i, events)
			snapshot, ok := i.Pause(rudderstackai.PauseOptions{})
			if !ok {
				t.Fatal("pause failed")
			}
			o.End()
			if snapshot.SDKTraceID != ownerTrace {
				t.Errorf("snapshot SDK trace %q, want the owner's %q", snapshot.SDKTraceID, ownerTrace)
			}
			if restore == "json" {
				data, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var stored rudderstackai.PausedInteraction
				if err := json.Unmarshal(data, &stored); err != nil {
					t.Fatal(err)
				}
				ctx, i = rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &stored})
			} else {
				i.Resume()
			}
			wrapperIn(ctx, t, i, events)
			i.End(rudderstackai.EndOptions{})
			for _, e := range spanEvents(events()) {
				if got := e["properties"].(object)["trace_id"]; got != ownerTrace {
					t.Errorf("span on trace %v, want %s", got, ownerTrace)
				}
			}
		})
	}
}

func TestSpanDedupPanickingFilterIsAUserCallbackError(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		if !s.EndTime().IsZero() {
			panic("filter exploded")
		}
		return IsAISpan(s)
	}})
	var mu sync.Mutex
	kinds := map[string]string{}
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true), rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
		mu.Lock()
		defer mu.Unlock()
		kinds[info.Where] = info.Kind
	})); err != nil {
		t.Fatal(err)
	}
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	wrapperIn(ctx, t, i, events)
	o.End()
	i.End(rudderstackai.EndOptions{})
	mu.Lock()
	defer mu.Unlock()
	for where, kind := range kinds {
		if strings.Contains(where, "filter") && kind != rudderstackai.KindUserCallback {
			t.Errorf("%s reported as %s, want user_callback", where, kind)
		}
	}
	if !slices.Contains(slices.Collect(maps.Values(kinds)), rudderstackai.KindUserCallback) {
		t.Errorf("the filter panic was not reported as user_callback: %v", kinds)
	}
	if spans := spanEvents(events()); len(spans) != 1 {
		t.Errorf("want the SDK span in the rejected owner's place, got %d", len(spans))
	}
}
