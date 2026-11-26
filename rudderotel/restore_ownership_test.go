package rudderotel

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func restoreOf(t *testing.T, i *rudderstackai.Interaction) *rudderstackai.Interaction {
	t.Helper()
	snap, ok := i.Pause(rudderstackai.PauseOptions{})
	if !ok {
		t.Fatal("pause refused")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var st rudderstackai.PausedInteraction
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	_, r := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &st, Classify: rudderstackai.Ptr(false)})
	return r
}

func llmAttrs() trace.SpanStartOption {
	return trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"),
		attribute.String("gen_ai.request.model", "m"), attribute.Int("gen_ai.usage.input_tokens", 5), attribute.Int("gen_ai.usage.output_tokens", 2))
}

func countEvents(events []object, name string) int {
	n := 0
	for _, e := range events {
		if e["event"] == name {
			n++
		}
	}
	return n
}

// An OTel model span inside an in-flight wrapper whose run was paused and restored in process is
// covered by the wrapper's span: one export, one counted call.
func TestRestoredRunStillOwnsInFlightWrapperCall(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	configureSpans(t, rudderstackai.WithLLMCallEvents(true))
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	started := time.Now()
	wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
	r := restoreOf(t, i)
	_, sp := provider.Tracer("app").Start(wrapped, "provider", llmAttrs())
	sp.End()
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", InputTokens: 5, OutputTokens: 2, StartedAt: started, EndedAt: time.Now()}, s, 200)
	r.End(rudderstackai.EndOptions{})
	i.End(rudderstackai.EndOptions{})
	rudderstackai.Flush(context.Background())
	ev := events()
	if names := spanNames(ev); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
		t.Errorf("spans %v, want one chat m and no provider", names)
	}
	if n := countEvents(ev, "ai_llm_response_received"); n != 1 {
		t.Errorf("ai_llm_response_received = %d, want 1", n)
	}
}

// A wrapper call on a context saved before a restore adopts the outer OTel model span: one export.
func TestRestoredRunWrapperAdoptsOuterOTelSpan(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	configureSpans(t, rudderstackai.WithLLMCallEvents(true))
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	outerCtx, outer := provider.Tracer("app").Start(ctx, "provider", llmAttrs())
	r := restoreOf(t, i)
	cur := rudderstackai.FromContext(outerCtx)
	if cur != r {
		t.Fatal("saved context does not follow the restore")
	}
	started := time.Now()
	_, s := hook.Tracing.BeginWrapper(outerCtx, cur, started)
	hook.Tracing.RecordWrapper(cur, rudderstackai.LLMCall{Provider: "openai", Model: "m", InputTokens: 5, OutputTokens: 2, StartedAt: started, EndedAt: time.Now()}, s, 200)
	outer.End()
	r.End(rudderstackai.EndOptions{})
	i.End(rudderstackai.EndOptions{})
	rudderstackai.Flush(context.Background())
	ev := events()
	if names := spanNames(ev); countName(names, "provider")+countName(names, "chat m") != 1 {
		t.Errorf("spans %v, want exactly one of provider / chat m", names)
	}
	if n := countEvents(ev, "ai_llm_response_received"); n != 1 {
		t.Errorf("ai_llm_response_received = %d, want 1", n)
	}
}

// JoinInteraction with ParentSpanID inside an active wrapper keeps the wrapper's ancestry, so the
// wrapper's call still owns the provider span.
func TestJoinParentSpanIDInsideWrapperKeepsOwnership(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	configureSpans(t, rudderstackai.WithLLMCallEvents(true))
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	_, root := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "root", Operation: "invoke_agent"})
	root.End(nil)
	started := time.Now()
	wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
	joined := rudderstackai.JoinInteraction(wrapped, i.ID(), rudderstackai.JoinOptions{ParentSpanID: root.SpanID})
	_, sp := provider.Tracer("app").Start(joined, "provider", llmAttrs())
	sp.End()
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", InputTokens: 5, OutputTokens: 2, StartedAt: started, EndedAt: time.Now()}, s, 200)
	i.End(rudderstackai.EndOptions{})
	rudderstackai.Flush(context.Background())
	ev := events()
	if names := spanNames(ev); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
		t.Errorf("spans %v, want one chat m and no provider", names)
	}
	if n := countEvents(ev, "ai_llm_response_received"); n != 1 {
		t.Errorf("ai_llm_response_received = %d, want 1", n)
	}
}
