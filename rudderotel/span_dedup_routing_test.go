package rudderotel

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// routedRun runs one local wrapper call and one OTel model span that names interaction named,
// with the span inside the call (inner) or around it (outer). It returns the exported span
// events and the OTel span.
func routedRun(t *testing.T, simple, inner bool, named func(local, foreign *rudderstackai.Interaction) string, endForeign bool) ([]object, trace.Span, []object) {
	t.Helper()
	provider, _, events := testProvider(t, Options{Simple: simple, ProductEvents: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	_, foreign := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "foreign-user", ConversationID: "foreign-conversation"})
	ctx, local := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "local-user"})
	id := named(local, foreign)
	if endForeign {
		foreign.End(rudderstackai.EndOptions{})
	}
	start := func(ctx context.Context) (context.Context, trace.Span) {
		return provider.Tracer("app").Start(ctx, "foreign chat", trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"),
			attribute.String("gen_ai.request.model", "foreign-model"), attribute.String("gen_ai.response.id", "foreign-response"),
			attribute.Int("gen_ai.usage.input_tokens", 6), attribute.Int("gen_ai.usage.output_tokens", 4),
			attribute.String("rudderstack.interaction_id", id), attribute.String("user.id", "foreign-user"),
		))
	}
	call := func(ctx context.Context, during func(context.Context)) {
		wctx, s := hook.Tracing.BeginWrapper(ctx, local, time.Now())
		if during != nil {
			during(wctx)
		}
		hook.Tracing.RecordWrapper(local, rudderstackai.LLMCall{Provider: "openai", Model: "local-model", ResponseID: "local-response", InputTokens: 2, OutputTokens: 1}, s, 200)
	}
	var o trace.Span
	if inner {
		call(ctx, func(ctx context.Context) {
			ctx, o = start(ctx)
			_ = ctx
			o.End()
		})
	} else {
		var octx context.Context
		octx, o = start(ctx)
		call(octx, nil)
		o.End()
	}
	local.End(rudderstackai.EndOptions{})
	foreign.End(rudderstackai.EndOptions{})
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	answers := []object{}
	for _, e := range events() {
		if e["event"] == "ai_response_received" {
			answers = append(answers, e)
		}
	}
	return spanEvents(events()), o, answers
}

// A span the app routed to another open interaction stays that interaction's: the ambient call
// neither covers nor adopts it, so both spans are sent once and product events route as before.
func TestSpanDedupKeepsASpanRoutedToAnotherInteraction(t *testing.T) {
	for _, simple := range []bool{true, false} {
		for _, inner := range []bool{true, false} {
			t.Run("simple="+strconvBool(simple)+"/inner="+strconvBool(inner), func(t *testing.T) {
				spans, o, answers := routedRun(t, simple, inner, func(_, foreign *rudderstackai.Interaction) string { return foreign.ID() }, false)
				foreignSpans, sdkSpans := 0, 0
				for _, e := range spans {
					p := e["properties"].(object)
					switch {
					case p["span_name"] == "foreign chat" && p["span_id"] == o.SpanContext().SpanID().String():
						foreignSpans++
					case strings.Contains(render(e), "rudderstack.span_source"):
						sdkSpans++
						if p["span_id"] == o.SpanContext().SpanID().String() {
							t.Error("the local call adopted the foreign span's IDs")
						}
					}
				}
				if foreignSpans != 1 || sdkSpans != 1 || len(spans) != 2 {
					t.Fatalf("foreign=%d sdk=%d total=%d, want one of each", foreignSpans, sdkSpans, len(spans))
				}
				for _, a := range answers {
					p := a["properties"].(object)
					if p["llm_call_count"] != 1.0 {
						t.Errorf("%v answer counts %v calls, want 1", a["userId"], p["llm_call_count"])
					}
				}
			})
		}
	}
}

// An ID no open interaction has, or an ended one, falls back to the ambient interaction, as does
// the ambient interaction's own ID: dedup as before, one SDK span for the call.
func TestSpanDedupFallsBackToTheAmbientInteraction(t *testing.T) {
	for _, c := range []struct {
		name  string
		named func(local, foreign *rudderstackai.Interaction) string
		ended bool
	}{
		{"unknown", func(_, _ *rudderstackai.Interaction) string { return "unknown-interaction" }, false},
		{"ended", func(_, foreign *rudderstackai.Interaction) string { return foreign.ID() }, true},
		{"ambient", func(local, _ *rudderstackai.Interaction) string { return local.ID() }, false},
	} {
		for _, inner := range []bool{true, false} {
			t.Run(c.name+"/inner="+strconvBool(inner), func(t *testing.T) {
				spans, _, _ := routedRun(t, true, inner, c.named, c.ended)
				if len(spans) != 1 || !strings.Contains(render(spans[0]), "rudderstack.span_source") {
					t.Fatalf("want only the SDK span, got %d", len(spans))
				}
			})
		}
	}
}
