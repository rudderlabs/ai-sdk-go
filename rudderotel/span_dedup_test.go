package rudderotel

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
		ctx, s := tracer.Start(ctx, n.Label, trace.WithAttributes(attribute.String("gen_ai.operation.name", n.Operation), attribute.String("gen_ai.system", "openai")))
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
