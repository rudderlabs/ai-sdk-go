package rudderotel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// The join synthetic parent is not a call: it neither covers an OTel model span by itself nor
// hides the call enclosing the span.
func TestSpanCoverageJoinParent(t *testing.T) {
	setup := func(t *testing.T) (context.Context, *rudderstackai.Interaction, trace.Tracer, func() []object, string) {
		provider, _, events := testProvider(t, Options{Simple: true})
		configureSpans(t)
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
		_, parent := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "root", Operation: "invoke_agent"})
		parent.End(nil)
		return rudderstackai.JoinInteraction(context.Background(), i.ID(), rudderstackai.JoinOptions{ParentSpanID: parent.SpanID}), i, provider.Tracer("app"), events, parent.SpanID
	}
	t.Run("call inside a join covers", func(t *testing.T) {
		joined, i, tracer, events, _ := setup(t)
		started := time.Now()
		wrapped, s := hook.Tracing.BeginWrapper(joined, i, started)
		modelSpan(wrapped, tracer, "provider").End()
		hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
		i.End(rudderstackai.EndOptions{})
		if names := spanNames(events()); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
			t.Errorf("spans %v", names)
		}
	})
	t.Run("manual middleware inside a joined call still covers", func(t *testing.T) {
		joined, i, tracer, events, _ := setup(t)
		started := time.Now()
		wrapped, s := hook.Tracing.BeginWrapper(joined, i, started)
		mw, manual := rudderstackai.StartSpan(wrapped, rudderstackai.SpanOptions{Name: "mw", Operation: "execute_tool"})
		modelSpan(mw, tracer, "provider").End()
		manual.End(nil)
		hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
		i.End(rudderstackai.EndOptions{})
		if names := spanNames(events()); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
			t.Errorf("spans %v", names)
		}
	})
	t.Run("a join alone does not cover", func(t *testing.T) {
		joined, i, tracer, events, _ := setup(t)
		modelSpan(joined, tracer, "provider").End()
		i.End(rudderstackai.EndOptions{})
		if names := spanNames(events()); countName(names, "provider") != 1 {
			t.Errorf("spans %v, want the OTel span exported", names)
		}
	})
	t.Run("another interaction's call does not cover", func(t *testing.T) {
		provider, _, events := testProvider(t, Options{Simple: true})
		configureSpans(t)
		ctxA, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "a"})
		_, b := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "b"})
		wrapped, s := hook.Tracing.BeginWrapper(ctxA, a, time.Now())
		inB := rudderstackai.JoinInteraction(wrapped, b.ID(), rudderstackai.JoinOptions{})
		modelSpan(inB, provider.Tracer("app"), "provider").End()
		hook.Tracing.FinishRaw(s, "openai", "m", 200, time.Now(), nil)
		a.End(rudderstackai.EndOptions{})
		b.End(rudderstackai.EndOptions{})
		if names := spanNames(events()); countName(names, "provider") != 1 {
			t.Errorf("spans %v, want the OTel span exported", names)
		}
	})
}
