package rudderotel

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

type dedupNode struct {
	Kind              string      `json:"kind"`
	Label             string      `json:"label"`
	Operation         string      `json:"operation"`
	EndBeforeChildren bool        `json:"end_before_children"`
	Legacy            bool        `json:"legacy"`
	AnonymousID       string      `json:"anonymous_id"`
	ConversationID    string      `json:"conversation_id"`
	StartTimeMS       int64       `json:"start_time_ms"`
	EndTimeMS         int64       `json:"end_time_ms"`
	Initial           attrValues  `json:"initial_attributes"`
	Completion        attrValues  `json:"completion_attributes"`
	ChildBefore       *dedupChild `json:"child_before_processor"`
	Children          []dedupNode `json:"children"`
}

type dedupChild struct {
	Label     string `json:"label"`
	Operation string `json:"operation"`
}

// attrValues are fixture attributes; {"repeat": n, "count": c} is a fixed array of c values n.
type attrValues map[string]any

func (a attrValues) expand() map[string]any {
	number := func(v any) (int64, bool) {
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		}
		return 0, false
	}
	out := map[string]any{}
	for k, v := range a {
		if o, ok := v.(map[string]any); ok {
			if count, ok := number(o["count"]); ok {
				repeat, _ := number(o["repeat"])
				values := make([]int64, count)
				for i := range values {
					values[i] = repeat
				}
				out[k] = values
				continue
			}
		}
		if i, ok := number(v); ok {
			out[k] = i
			continue
		}
		out[k] = v
	}
	return out
}

type dedupScenario struct {
	Name           string    `json:"name"`
	EmitSpans      bool      `json:"emit_spans"`
	CaptureContent bool      `json:"capture_content"`
	Processor      string    `json:"processor"`
	Requires       string    `json:"requires"`
	Tree           dedupNode `json:"tree"`
	ExpectedSpans  []struct {
		Label         string   `json:"label"`
		Parent        *string  `json:"parent"`
		Replaces      string   `json:"replaces"`
		AttributeKeys []string `json:"attribute_keys"`
		ShedKeys      []string `json:"shed_attribute_keys"`
		Truncated     *bool    `json:"truncated"`
	} `json:"expected_spans"`
	CallSpan           any      `json:"call_span"`
	OtherProcessorsSee []string `json:"other_processors_see"`
}

// wrapperOnlyKeys are attributes Go's only SDK model emitter, the provider wrapper, always sets.
// The fixture's model-emitter (integration) nodes run through it, so attribute key checks allow them.
var wrapperOnlyKeys = []string{"gen_ai.response.model", "http.response.status_code"}

// dedupRun drives one scenario tree: interactions, wrapper calls (children run inside the
// provider request, as an instrumentation would), model emitters, manual spans and active OTel
// spans.
func dedupRun(ctx context.Context, tracer trace.Tracer, n dedupNode) {
	children := func(ctx context.Context) {
		for _, c := range n.Children {
			dedupRun(ctx, tracer, c)
		}
	}
	switch n.Kind {
	case "interaction":
		ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: n.Label, AnonymousID: n.AnonymousID, ConversationID: n.ConversationID})
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
	case "integration":
		// An SDK model emitter with set times and attributes: Go's is the provider wrapper.
		i := rudderstackai.FromContext(ctx)
		started, ended := time.Now(), time.Time{}
		if n.StartTimeMS > 0 {
			started, ended = time.UnixMilli(n.StartTimeMS), time.UnixMilli(n.EndTimeMS)
		}
		ctx, value := hook.Tracing.BeginWrapper(ctx, i, started)
		s, _ := value.(*rudderstackai.Span)
		initial := n.Initial.expand()
		s.SetAttributes(initial)
		children(ctx)
		s.SetAttributes(n.Completion.expand())
		provider, model := "openai", n.Label
		if v, ok := initial["gen_ai.provider.name"].(string); ok {
			provider = v
		}
		if v, ok := initial["gen_ai.request.model"].(string); ok {
			model = v
		}
		if ended.IsZero() {
			ended = time.Now()
		}
		hook.Tracing.FinishRaw(value, provider, model, 0, ended, nil)
	}
}

// earlyProcessor starts and ends a child synchronously in its OnStart for span target, with the
// model active, before RudderStack's processor sees the model span.
type earlyProcessor struct {
	target string
	child  *dedupChild
	tracer func() trace.Tracer
}

func (e *earlyProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	if e.child == nil || s.Name() != e.target {
		return
	}
	_, child := e.tracer().Start(trace.ContextWithSpan(ctx, s), e.child.Label, trace.WithAttributes(attribute.String("gen_ai.operation.name", e.child.Operation)))
	child.End()
}
func (e *earlyProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (e *earlyProcessor) Shutdown(context.Context) error   { return nil }
func (e *earlyProcessor) ForceFlush(context.Context) error { return nil }

func findChildBefore(n dedupNode) (string, *dedupChild) {
	if n.ChildBefore != nil {
		return n.Label, n.ChildBefore
	}
	for _, c := range n.Children {
		if label, child := findChildBefore(c); child != nil {
			return label, child
		}
	}
	return "", nil
}

func TestSharedSpanDedupFixture(t *testing.T) {
	var fixture struct {
		FixtureVersion int             `json:"fixture_version"`
		Scenarios      []dedupScenario `json:"scenarios"`
	}
	loadCases(t, "../spec/fixtures/span-dedup-v0.1.json", &fixture)
	if fixture.FixtureVersion != 2 {
		t.Fatalf("fixture_version %d, want 2", fixture.FixtureVersion)
	}
	for _, sc := range fixture.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Requires == "integration" {
				t.Skip("platform limit: Go has no framework integrations")
			}
			apm := tracetest.NewSpanRecorder()
			// The tested processor sits between an early processor and a plain one.
			_, p, events := testProvider(t, Options{Simple: sc.Processor != "batch"})
			target, child := findChildBefore(sc.Tree)
			var tracer trace.Tracer
			early := &earlyProcessor{target: target, child: child, tracer: func() trace.Tracer { return tracer }}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(early), sdktrace.WithSpanProcessor(p), sdktrace.WithSpanProcessor(apm))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			tracer = provider.Tracer("app")
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(sc.EmitSpans), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(sc.CaptureContent)); err != nil {
				t.Fatal(err)
			}
			dedupRun(context.Background(), tracer, sc.Tree)
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}

			labels := map[string]string{}
			otel := map[string]sdktrace.ReadOnlySpan{}
			seen := []string{}
			for _, s := range apm.Ended() {
				labels[s.SpanContext().SpanID().String()] = s.Name()
				otel[s.Name()] = s
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
					if want.AttributeKeys != nil {
						checkBudgetSpan(t, e, want.AttributeKeys, want.ShedKeys, want.Truncated)
					}
					if want.Replaces != "" {
						// The SDK span carries the OTel span's IDs; its content is the SDK call's.
						o := otel[want.Replaces]
						if o == nil {
							t.Fatalf("%s replaces %s, which never ended", want.Label, want.Replaces)
						}
						wantParent := ""
						if o.Parent().IsValid() {
							wantParent = o.Parent().SpanID().String()
						}
						if p["trace_id"] != o.SpanContext().TraceID().String() || p["span_id"] != o.SpanContext().SpanID().String() || parent != wantParent {
							t.Errorf("%s IDs %v/%v/%v, want %s's", want.Label, p["trace_id"], p["span_id"], parent, want.Replaces)
						}
						if !strings.Contains(render(e), "rudderstack.span_source") {
							t.Errorf("%s is not the SDK's span", want.Label)
						}
						continue
					}
					switch {
					case want.Parent == nil && parent != "":
						t.Errorf("%s: parent %q, want none", want.Label, labels[parent])
					case want.Parent != nil && labels[parent] != *want.Parent:
						t.Errorf("%s: parent %q, want %q", want.Label, labels[parent], *want.Parent)
					}
					// A child shares its parent's trace, including a covered subtree moved under its owner.
					if want.Parent != nil {
						for _, other := range exported {
							if op := other["properties"].(object); label(op["span_name"]) == *want.Parent && op["trace_id"] != p["trace_id"] {
								t.Errorf("%s is on trace %v, its parent %s on %v", want.Label, p["trace_id"], *want.Parent, op["trace_id"])
							}
						}
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
			// call_span: one label for every call event, or an ordered list for successive ones.
			var wantCalls []string
			switch v := sc.CallSpan.(type) {
			case string:
				wantCalls = []string{v}
			case []any:
				for _, l := range v {
					wantCalls = append(wantCalls, l.(string))
				}
			}
			n := 0
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
				switch {
				case len(wantCalls) == 1:
					want = wantCalls[0]
				case n < len(wantCalls):
					want = wantCalls[n]
				}
				if got != want {
					t.Errorf("call %d span_id is %q, want %q", n, got, want)
				}
				n++
			}
			if len(wantCalls) > 1 && n != len(wantCalls) {
				t.Errorf("%d call events, want %d", n, len(wantCalls))
			}
		})
	}
}

// checkBudgetSpan checks a shed SDK span: its OTLP span attribute keys, the removed keys, the
// core summary and timing, and that it fits the default budget.
func checkBudgetSpan(t *testing.T, e object, keys, shed []string, truncated *bool) {
	t.Helper()
	p := e["properties"].(object)
	span := p["otlp"].(object)["resourceSpans"].([]any)[0].(object)["scopeSpans"].([]any)[0].(object)["spans"].([]any)[0].(object)
	got := []string{}
	for _, kv := range span["attributes"].([]any) {
		if k := kv.(object)["key"].(string); !slices.Contains(wrapperOnlyKeys, k) {
			got = append(got, k)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, keys) {
		t.Errorf("attribute keys %v, want %v", got, keys)
	}
	for _, k := range shed {
		if slices.Contains(got, k) {
			t.Errorf("%s was not shed", k)
		}
	}
	if truncated != nil && p["truncated"] != *truncated {
		t.Errorf("truncated=%v, want %v", p["truncated"], *truncated)
	}
	summary, _ := p["gen_ai"].(object)
	if p["start_time"] != "2025-10-09T08:53:20.000Z" || p["end_time"] != "2025-10-09T08:53:21.500Z" || p["duration_ms"] != 1500.0 || p["status_code"] != "ok" ||
		p["conversation_id"] != "conversation" || summary["operation_name"] != "chat" || summary["provider_name"] != "openai" || summary["request_model"] != "m" {
		t.Errorf("core properties changed: %v", p)
	}
	if e["userId"] != "A" || e["anonymousId"] != "anon" || e["originalTimestamp"] == nil {
		t.Errorf("identity or timestamp lost: %v/%v/%v", e["userId"], e["anonymousId"], e["originalTimestamp"])
	}
	if b, _ := json.Marshal(e); len(b) > 32_000 {
		t.Errorf("delivered %d bytes, over the 32000 budget", len(b))
	}
}

// ownerSpan starts an active OTel model-call span in a new interaction of user u.
func ownerSpan(tracer trace.Tracer) (context.Context, *rudderstackai.Interaction, trace.Span) {
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, o := tracer.Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	return ctx, i, o
}

func rejectErrors(s sdktrace.ReadOnlySpan) bool { return IsAISpan(s) && s.Status().Code != codes.Error }

// render renders an event for substring checks.
func render(e object) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func isSDKSpan(e object) bool { return strings.Contains(render(e), "rudderstack.span_source") }

func configureSpans(t *testing.T, options ...rudderstackai.ConfigOption) {
	t.Helper()
	if err := rudderstackai.Configure(append([]rudderstackai.ConfigOption{rudderstackai.WithEmitSpans(true)}, options...)...); err != nil {
		t.Fatal(err)
	}
}

// wrapperIn runs one wrapper call in ctx and returns the span ID it sent.
func wrapperIn(ctx context.Context, t *testing.T, i *rudderstackai.Interaction) string {
	t.Helper()
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
	return s.(*rudderstackai.Span).SpanID
}

// requireOneSDKSpanUnder asserts a single exported span: the SDK's, under o's IDs and parent.
func requireOneSDKSpanUnder(t *testing.T, events func() []object, o trace.Span, parent string) object {
	t.Helper()
	spans := spanEvents(events())
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	p := spans[0]["properties"].(object)
	ids := o.SpanContext()
	if p["trace_id"] != ids.TraceID().String() || p["span_id"] != ids.SpanID().String() || (parent != "" && p["parent_span_id"] != parent) {
		t.Errorf("span IDs %v/%v/%v, want the OTel span's %s/%s/%s", p["trace_id"], p["span_id"], p["parent_span_id"], ids.TraceID(), ids.SpanID(), parent)
	}
	if !isSDKSpan(spans[0]) {
		t.Error("the exported span is not the SDK's")
	}
	return p
}

func TestSpanDedupSDKSpanReplacesTheAppSpan(t *testing.T) {
	apm := tracetest.NewSpanRecorder()
	provider, _, events := testProvider(t, Options{Simple: true}, apm)
	configureSpans(t, rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(true))
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, root := provider.Tracer("app").Start(ctx, "request")
	ctx, app := provider.Tracer("app").Start(ctx, "chat gpt-4o-mini", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("app.attr", "kept")))
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 5, OutputText: "hi", InputMessages: []rudderstackai.GenAIMessage{{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "hello"}}}}}, s, 200)
	app.End()
	root.End()
	i.End(rudderstackai.EndOptions{})
	p := requireOneSDKSpanUnder(t, events, app, root.SpanContext().SpanID().String())
	// RudderStack gets the SDK span's content, not the app span's attributes.
	if strings.Contains(render(spanEvents(events())[0]), "app.attr") {
		t.Error("the app span's attributes reached RudderStack")
	}
	if p["span_name"] != "chat gpt-4o-mini" {
		t.Errorf("span name %v", p["span_name"])
	}
	for _, e := range apm.Ended() {
		if e.Name() != "chat gpt-4o-mini" {
			continue
		}
		for _, kv := range e.Attributes() {
			if strings.HasPrefix(string(kv.Key), "gen_ai.usage.") || strings.HasPrefix(string(kv.Key), "gen_ai.input.") || kv.Key == "rudderstack.span_source" {
				t.Errorf("the SDK wrote %s onto the app span", kv.Key)
			}
		}
	}
}

// A non-RudderStack processor receives the OTel span unchanged: same IDs, name, parent and
// attributes, ended once.
func TestSpanDedupOtherProcessorReceivesTheOTelSpanUnchanged(t *testing.T) {
	apm := tracetest.NewSpanRecorder()
	provider, _, events := testProvider(t, Options{Simple: true}, apm)
	configureSpans(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	ctx, root := provider.Tracer("app").Start(ctx, "request")
	ctx, app := provider.Tracer("app").Start(ctx, "chat gpt-4o-mini", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("app.attr", "kept")))
	wrapperIn(ctx, t, i)
	app.End()
	root.End()
	i.End(rudderstackai.EndOptions{})
	var got []sdktrace.ReadOnlySpan
	for _, s := range apm.Ended() {
		if s.Name() == "chat gpt-4o-mini" {
			got = append(got, s)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the other processor received the OTel span %d times", len(got))
	}
	s := got[0]
	if s.SpanContext().TraceID() != app.SpanContext().TraceID() || s.SpanContext().SpanID() != app.SpanContext().SpanID() || s.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("IDs changed: %v parent %v", s.SpanContext(), s.Parent())
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["app.attr"] != "kept" || attrs["gen_ai.operation.name"] != "chat" {
		t.Errorf("attributes changed: %v", attrs)
	}
	for k := range attrs {
		if strings.HasPrefix(k, "gen_ai.usage.") || k == "rudderstack.span_source" {
			t.Errorf("the SDK wrote %s onto the app span", k)
		}
	}
	if n := len(spanEvents(events())); n != 1 {
		t.Errorf("RudderStack got %d spans, want the SDK span alone", n)
	}
}

// recordOnly samples spans as recording but not sampled: the processor sees them, the exporter never does.
type recordOnly struct{}

func (recordOnly) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return sdktrace.SamplingResult{Decision: sdktrace.RecordOnly, Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState()}
}
func (recordOnly) Description() string { return "record-only" }

// Adoption does not depend on what the processor would export: a filtered-out or unsampled span
// is replaced too, and the call still has exactly one span.
func TestSpanDedupAdoptsFilteredAndUnsampledSpans(t *testing.T) {
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
			configureSpans(t, rudderstackai.WithLLMCallEvents(true))
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			wrapperIn(ctx, t, i)
			o.End()
			i.End(rudderstackai.EndOptions{})
			requireOneSDKSpanUnder(t, events, o, "")
		})
	}
}

func TestSpanDedupEndedInteractionNeverJoins(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
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
	configureSpans(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer i.End(rudderstackai.EndOptions{})
	// One trace throughout: the manual span is under an OTel request span, and so is the model span.
	ctx, root := provider.Tracer("app").Start(ctx, "request")
	defer root.End()
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

// A shut-down processor never replaces a span: it exports nothing, so the span is its own.
func TestSpanDedupShutdownForgetsItsSpans(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	defer o.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id := wrapperIn(ctx, t, i); id == o.SpanContext().SpanID().String() {
		t.Error("a span of a shut-down processor was adopted")
	}
	if n := len(spanEvents(events())); n != 1 {
		t.Errorf("want the SDK span alone, got %d", n)
	}
}

// The SDK span is sent when its call finishes, whatever the OTel span's filter, status or timing.
func TestSpanDedupSDKSpanIsSentWhateverTheOTelSpanDoes(t *testing.T) {
	for _, order := range []string{"after", "before"} {
		t.Run("OTel ends "+order+" the call", func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, Filter: rejectErrors})
			configureSpans(t, rudderstackai.WithLLMCallEvents(true))
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
			if n := len(spanEvents(events())); n != 1 {
				t.Fatalf("sent %d spans when the call finished, want the SDK span", n)
			}
			if order == "after" {
				o.SetStatus(codes.Error, "")
				o.End()
			}
			root.End()
			i.End(rudderstackai.EndOptions{})
			requireOneSDKSpanUnder(t, events, o, root.SpanContext().SpanID().String())
			for _, e := range events() {
				if e["event"] == "ai_llm_response_received" && e["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
					t.Errorf("call span_id %v, want the adopted ID", e["properties"].(object)["span_id"])
				}
			}
		})
	}
}

// The export filter is never asked about a replaced span: not at start, not at end.
func TestSpanDedupNeverCallsTheFilterForAReplacedSpan(t *testing.T) {
	var calls atomic.Int32
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(s sdktrace.ReadOnlySpan) bool {
		if s.Name() == "chat" {
			calls.Add(1)
			panic("filter exploded")
		}
		return true
	}})
	var mu sync.Mutex
	var reported []string
	configureSpans(t, rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, info.Kind+":"+info.Where)
	}))
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	wrapperIn(ctx, t, i)
	o.End()
	i.End(rudderstackai.EndOptions{})
	if n := calls.Load(); n != 0 {
		t.Errorf("the filter ran %d times for a replaced span", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 0 {
		t.Errorf("reported %v", reported)
	}
	requireOneSDKSpanUnder(t, events, o, "")
}

// An OTel model-call span with no SDK call inside is exported as before, and a panicking
// filter on it is a user_callback error.
func TestSpanDedupPanickingFilterIsAUserCallbackError(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, Filter: func(sdktrace.ReadOnlySpan) bool { panic("filter exploded") }})
	var mu sync.Mutex
	kinds := map[string]string{}
	configureSpans(t, rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
		mu.Lock()
		defer mu.Unlock()
		kinds[info.Where] = info.Kind
	}))
	_, _, o := ownerSpan(provider.Tracer("app"))
	o.End()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(slices.Collect(maps.Values(kinds)), rudderstackai.KindUserCallback) {
		t.Errorf("the filter panic was not reported as user_callback: %v", kinds)
	}
	for where, kind := range kinds {
		if strings.Contains(where, "filter") && kind != rudderstackai.KindUserCallback {
			t.Errorf("%s reported as %s, want user_callback", where, kind)
		}
	}
	if n := len(spanEvents(events())); n != 0 {
		t.Errorf("a span whose filter panicked was exported: %d", n)
	}
}

func TestSpanDedupFlushAndShutdownFinishAnUnreadCall(t *testing.T) {
	for _, method := range []string{"Flush", "Shutdown"} {
		t.Run(method, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			configureSpans(t)
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			hook.Tracing.BeginWrapper(ctx, i, time.Now()) // an unread call
			finish := rudderstackai.Flush
			if method == "Shutdown" {
				finish = rudderstackai.Shutdown
			}
			if !finish(context.Background()) {
				t.Fatalf("%s did not finish", method)
			}
			o.End()
			i.End(rudderstackai.EndOptions{})
			requireOneSDKSpanUnder(t, events, o, "")
		})
	}
}

// Every processor that saw the span drops it, whatever each one's filter says, and the call has
// one span.
func TestSpanDedupEveryProcessorDropsTheReplacedSpan(t *testing.T) {
	accept := func(sdktrace.ReadOnlySpan) bool { return true }
	reject := func(sdktrace.ReadOnlySpan) bool { return false }
	errorsOnly := func(s sdktrace.ReadOnlySpan) bool { return s.Status().Code == codes.Error }
	for _, c := range []struct {
		name    string
		filters [2]func(sdktrace.ReadOnlySpan) bool
	}{
		{"both accept", [2]func(sdktrace.ReadOnlySpan) bool{accept, accept}},
		{"one accepts, one rejects", [2]func(sdktrace.ReadOnlySpan) bool{accept, reject}},
		{"both reject", [2]func(sdktrace.ReadOnlySpan) bool{reject, reject}},
		{"successes and errors", [2]func(sdktrace.ReadOnlySpan) bool{rejectErrors, errorsOnly}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, p1, events := testProvider(t, Options{Simple: true, Filter: c.filters[0]})
			configureSpans(t)
			p2, err := NewSpanProcessor(Options{Simple: true, Filter: c.filters[1], Analytics: p1.exporter.client})
			if err != nil {
				t.Fatal(err)
			}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(p2))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			wrapperIn(ctx, t, i)
			o.SetStatus(codes.Error, "")
			o.End()
			i.End(rudderstackai.EndOptions{})
			requireOneSDKSpanUnder(t, events, o, "")
		})
	}
}

// probeProcessor runs fn when a span ends, in registration order among the processors.
type probeProcessor struct{ onEnd func(sdktrace.ReadOnlySpan) }

func (probeProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p probeProcessor) OnEnd(s sdktrace.ReadOnlySpan)                 { p.onEnd(s) }
func (probeProcessor) Shutdown(context.Context) error                  { return nil }
func (probeProcessor) ForceFlush(context.Context) error                { return nil }

// stillRecording reports a span as recording whatever its state.
type stillRecording struct{ trace.Span }

func (stillRecording) IsRecording() bool { return true }

// Once a processor reached a span's end without a replacement, the decision is final: a call
// starting while a later processor still ends it keeps its own span, so the span is never both
// exported by the first and replaced for the second.
func TestSpanDedupTheFirstEndMakesTheDecisionFinal(t *testing.T) {
	_, p1, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	var ctx context.Context
	var i *rudderstackai.Interaction
	var late string
	// The call's context still shows the span as recording, as it would to a call that checked
	// just before the span ended.
	probe := probeProcessor{onEnd: func(sdktrace.ReadOnlySpan) {
		late = wrapperIn(trace.ContextWithSpan(ctx, stillRecording{trace.SpanFromContext(ctx)}), t, i)
	}}
	// p1 (exports) ends the span first; the probe starts a call; p2 (rejects everything) ends it last.
	p2, err := NewSpanProcessor(Options{Simple: true, Filter: func(sdktrace.ReadOnlySpan) bool { return false }, Analytics: p1.exporter.client})
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(probe), sdktrace.WithSpanProcessor(p2))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var o trace.Span
	ctx, i, o = ownerSpan(provider.Tracer("app"))
	o.End()
	i.End(rudderstackai.EndOptions{})
	spans := spanEvents(events())
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s["properties"].(object)["span_id"].(string)] = true
	}
	if len(spans) != 2 || len(ids) != 2 || !ids[o.SpanContext().SpanID().String()] || late == "" || late == o.SpanContext().SpanID().String() {
		t.Fatalf("want the OTel span and the call's own span, got %d spans %v (late call %s)", len(spans), ids, late)
	}
}

// An adoption racing the span's end ends with exactly one span under the OTel span's ID, never
// none and never two, even with several processors.
func TestSpanDedupAdoptionRacingEndNeverLosesOrDuplicates(t *testing.T) {
	_, p1, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	p2, err := NewSpanProcessor(Options{Simple: true, Filter: func(sdktrace.ReadOnlySpan) bool { return false }, Analytics: p1.exporter.client})
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(p2))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	const rounds = 200
	ids := make([]string, rounds)
	for r := range rounds {
		ctx, i, o := ownerSpan(provider.Tracer("app"))
		ids[r] = o.SpanContext().SpanID().String()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); wrapperIn(ctx, t, i) }()
		go func() { defer wg.Done(); o.End() }()
		wg.Wait()
		i.End(rudderstackai.EndOptions{})
	}
	byID := map[string]int{}
	total := 0
	for _, e := range spanEvents(events()) {
		byID[e["properties"].(object)["span_id"].(string)]++
		total++
	}
	for _, id := range ids {
		if byID[id] != 1 {
			t.Fatalf("%d spans under OTel span %s, want exactly 1", byID[id], id)
		}
	}
	if total < rounds || total > 2*rounds {
		t.Fatalf("%d spans for %d calls", total, rounds)
	}
}

// The shared mark is bounded by the open spans: gone once every processor ended the span or shut down.
func TestSpanDedupMarkIsRemovedAtTheSpansEnd(t *testing.T) {
	size := func() int {
		otelCalls.mu.Lock()
		defer otelCalls.mu.Unlock()
		return len(otelCalls.m)
	}
	_, p1, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	p2, err := NewSpanProcessor(Options{Simple: true, Analytics: p1.exporter.client})
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p1), sdktrace.WithSpanProcessor(p2))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	before := size()
	for _, replaced := range []bool{true, false} {
		ctx, i, o := ownerSpan(provider.Tracer("app"))
		if size() != before+1 {
			t.Fatalf("an open span left %d marks, want %d", size(), before+1)
		}
		if replaced {
			wrapperIn(ctx, t, i)
		}
		o.End()
		i.End(rudderstackai.EndOptions{})
		if size() != before {
			t.Fatalf("an ended span (replaced=%v) left %d marks, want %d", replaced, size(), before)
		}
	}
	// Shutdown forgets the marks of spans still open.
	_, i, o := ownerSpan(provider.Tracer("app"))
	defer o.End()
	defer i.End(rudderstackai.EndOptions{})
	if err := p1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if size() != before {
		t.Errorf("shut-down processors left %d marks, want %d", size(), before)
	}
	_ = events
}

func TestSpanDedupNoRegistrationAfterShutdown(t *testing.T) {
	_, p, _ := testProvider(t, Options{Simple: true})
	configureSpans(t)
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
		t.Error("a shut-down processor registered a span")
	}
}

// blockedClient blocks every Enqueue until release closes.
type blockedClient struct {
	*ruddertest.Recorder
	release chan struct{}
}

func (c *blockedClient) Enqueue(m analytics.Message) error {
	<-c.release
	return c.Recorder.Enqueue(m)
}

// Ending a replaced span never touches the processor's client, so a blocked client cannot block End.
func TestSpanDedupEndingAReplacedSpanNeverBlocksOnTheClient(t *testing.T) {
	for _, simple := range []bool{true, false} {
		t.Run("simple="+strconvBool(simple), func(t *testing.T) {
			r, err := ruddertest.NewRecorder(filepath.Join(t.TempDir(), "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			blocked := &blockedClient{Recorder: r, release: make(chan struct{})}
			defer func() { close(blocked.release); _ = r.Close() }()
			provider, _, events := testProvider(t, Options{Simple: simple, Analytics: blocked})
			configureSpans(t)
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			wrapperIn(ctx, t, i) // the SDK's own client is the unblocked recorder
			done := make(chan struct{})
			go func() { o.End(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("End blocked on the client")
			}
			i.End(rudderstackai.EndOptions{})
			requireOneSDKSpanUnder(t, events, o, "")
		})
	}
}

// The SDK call never waits for the batch queue: a full queue cannot cost the call its span.
func TestSpanDedupFullBatchQueueDoesNotAffectTheSDKSpan(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true, ExportAllSpans: true})
	// A batch queue of one, its exporter stuck on busy and its queue holding queued.
	blocked := &blockedExporter{SpanExporter: p.exporter, entered: make(chan struct{}), release: make(chan struct{})}
	p.inner = sdktrace.NewBatchSpanProcessor(blocked, sdktrace.WithMaxQueueSize(1), sdktrace.WithMaxExportBatchSize(1), sdktrace.WithBatchTimeout(time.Minute))
	configureSpans(t)
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
	wrapperIn(ctx, t, i)
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
			if e["properties"].(object)["span_id"] != o.SpanContext().SpanID().String() {
				t.Error("the call's span is not under the adopted ID")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("the call has %d spans, want 1", calls)
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

// A processor shut down by the client's callback while the SDK span is sent cannot duplicate it.
func TestSpanDedupShutdownWhileSendingNeverDuplicates(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	wrapperIn(ctx, t, i)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	o.End()
	i.End(rudderstackai.EndOptions{})
	requireOneSDKSpanUnder(t, events, o, "")
}

// The SDK span is sent when its call finishes even if the OTel span never ends.
func TestSpanDedupSDKSpanSendsBeforeAnOpenOTelSpanEnds(t *testing.T) {
	provider, p, events := testProvider(t, Options{Simple: true})
	configureSpans(t)
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	defer i.End(rudderstackai.EndOptions{})
	wrapperIn(ctx, t, i)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireOneSDKSpanUnder(t, events, o, "")
}

// The call finishing after the OTel span ended (a stream read later) still sends the adopted span.
func TestSpanDedupCallFinishingAfterTheOTelSpanEnded(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	configureSpans(t, rudderstackai.WithLLMCallEvents(true))
	ctx, i, o := ownerSpan(provider.Tracer("app"))
	_, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	o.End()
	if n := len(spanEvents(events())); n != 0 {
		t.Fatalf("%d spans before the call finished", n)
	}
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
	i.End(rudderstackai.EndOptions{})
	requireOneSDKSpanUnder(t, events, o, "")
}

// The first call adopts the OTel span, replacing any SDK trace the interaction already had; later
// calls inside are its children, and a later manual span stays on that trace.
func TestSpanDedupLaterCallsAreChildrenOfTheAdoptedSpan(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	configureSpans(t, rudderstackai.WithLLMCallEvents(true))
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	_, before := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "before", Operation: "execute_tool"})
	before.End(nil) // allocates the interaction's SDK trace
	octx, o := provider.Tracer("app").Start(ctx, "chat outer", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	if o.SpanContext().TraceID().String() == before.TraceID {
		t.Fatal("the OTel span is on the interaction's trace already")
	}
	first := wrapperIn(octx, t, i)
	second := wrapperIn(octx, t, i)
	o.End()
	_, after := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "after", Operation: "execute_tool"})
	after.End(nil)
	i.End(rudderstackai.EndOptions{})
	oid := o.SpanContext().SpanID().String()
	if first != oid || second == oid {
		t.Fatalf("first call %s, second %s, want the first under %s and the second not", first, second, oid)
	}
	traces := map[string]string{}
	for _, e := range spanEvents(events()) {
		p := e["properties"].(object)
		traces[p["span_name"].(string)] = p["trace_id"].(string)
		if p["span_id"] == second && p["parent_span_id"] != oid {
			t.Errorf("second call parent %v, want %s", p["parent_span_id"], oid)
		}
	}
	otrace := o.SpanContext().TraceID().String()
	for _, name := range []string{"chat w", "after"} {
		if traces[name] != otrace {
			t.Errorf("%s on trace %s, want the adopted %s", name, traces[name], otrace)
		}
	}
}

func TestSpanDedupAdoptedCallFixesTheSDKTraceAcrossPause(t *testing.T) {
	for _, restore := range []string{"in-process", "json"} {
		t.Run(restore, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			configureSpans(t)
			ctx, i, o := ownerSpan(provider.Tracer("app"))
			ownerTrace := o.SpanContext().TraceID().String()
			wrapperIn(ctx, t, i)
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
			wrapperIn(ctx, t, i)
			i.End(rudderstackai.EndOptions{})
			for _, e := range spanEvents(events()) {
				if got := e["properties"].(object)["trace_id"]; got != ownerTrace {
					t.Errorf("span on trace %v, want %s", got, ownerTrace)
				}
			}
		})
	}
}
