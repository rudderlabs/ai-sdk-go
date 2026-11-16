package rudderotel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestSpanRulesConcurrentNestedCalls(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	var work sync.WaitGroup
	for n := range 32 {
		work.Add(1)
		go func() {
			defer work.Done()
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: fmt.Sprint(n)})
			tracer := provider.Tracer("test")
			parentCtx, parent := tracer.Start(ctx, "parent", trace.WithAttributes(attribute.String("gen_ai.operation.name", "generate_content"), attribute.String("gen_ai.request.model", "model")))
			middleCtx, middle := tracer.Start(parentCtx, "non-model")
			_, child := tracer.Start(middleCtx, "child", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.response.id", fmt.Sprint(n)), attribute.Int("gen_ai.usage.input_tokens", 7)))
			parent.End()
			middle.End()
			child.End()
			i.End(rudderstackai.EndOptions{})
		}()
	}
	work.Wait()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, e := range events() {
		if e["event"] == "ai_response_received" {
			answers++
			props := e["properties"].(object)
			if props["llm_call_count"] != float64(1) || props["total_input_tokens"] != float64(7) {
				t.Fatal(props)
			}
		}
	}
	if answers != 32 {
		t.Fatalf("answers=%d want 32", answers)
	}
}

func TestSpanRulesConcurrentFlushCountsOnce(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	interactions := make([]*rudderstackai.Interaction, 16)
	for n := range interactions {
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: fmt.Sprint(n)})
		interactions[n] = i
		_, span := provider.Tracer("test").Start(ctx, "prep", trace.WithAttributes(attribute.String("openinference.span.kind", "LLM")))
		span.End()
	}
	var work sync.WaitGroup
	for range 8 {
		work.Add(1)
		go func() {
			defer work.Done()
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Error(err)
			}
			if !rudderstackai.Flush(context.Background()) {
				t.Error("core flush failed")
			}
		}()
	}
	work.Wait()
	for _, i := range interactions {
		i.End(rudderstackai.EndOptions{})
	}
	answers := 0
	for _, e := range events() {
		if e["event"] == "ai_response_received" {
			answers++
			if got := e["properties"].(object)["llm_call_count"]; got != float64(1) {
				t.Fatalf("calls=%v want 1", got)
			}
		}
	}
	if answers != 16 {
		t.Fatalf("answers=%d want 16", answers)
	}
}

func TestSpanRulesBoundedEvidenceCountsConservatively(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	r, events := recorder(t)
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r), rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1})); err != nil {
		t.Fatal(err)
	}
	p, err := NewSpanProcessor(Options{Analytics: r, Simple: true, ProductEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	for n := range 4 {
		f := fixtureSpan{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: fmt.Sprintf("%016x", n+1), Name: "prep", Start: fmt.Sprint(time.Now().UnixNano()), End: fmt.Sprint(time.Now().UnixNano()), Attributes: map[string]any{InteractionIDAttribute: i.ID(), "openinference.span.kind": "LLM"}}
		span, provider := startFixture(ctx, t, f, p)
		span.End()
		// Model calls record immediately even when ancestry is bounded.
		_ = provider

	}
	i.End(rudderstackai.EndOptions{})
	for _, e := range events() {
		if e["event"] == "ai_response_received" {
			if got := e["properties"].(object)["llm_call_count"]; got != float64(4) {
				t.Fatalf("calls=%v want 4", got)
			}
			return
		}
	}
	t.Fatal("missing answer")
}
