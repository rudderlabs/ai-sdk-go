package rudderstackai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type spanGetterPanicContext struct{ context.Context }

func (c spanGetterPanicContext) Value(any) any { panic("context getter") }

func TestManualSpanSetupFailureStillRunsWork(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	reports := 0
	if err := Configure(WithOnError(func(error, ErrorInfo) { reports++ })); err != nil {
		t.Fatal(err)
	}
	ctx := spanGetterPanicContext{context.Background()}
	returned, span := StartSpan(ctx, SpanOptions{Name: "lookup"})
	called := false
	func() { called = true; span.End(nil) }()
	if !called || returned != ctx || reports != 1 {
		t.Fatalf("work=%v context=%v reports=%d", called, returned, reports)
	}
}

type uncopyableSpanMapping map[string]any

func (uncopyableSpanMapping) MarshalJSON() ([]byte, error) { panic("must not serialize") }

func TestManualSpanContextSafelyCopiesMapping(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	mapping := uncopyableSpanMapping{"label": "before", "binary": []byte("private")}
	ctx := WithValues(context.Background(), ContextValues{UserID: "alice", Context: map[string]any{"traits": mapping}})
	_, span := StartSpan(ctx, SpanOptions{Name: "lookup"})
	mapping["label"] = "after"
	span.End(nil)
	events := r.named("ai_otel_span")
	if len(events) != 1 || events[0].UserId != "alice" {
		t.Fatal("mapping prevented span completion")
	}
	traits := events[0].Context.Extra["traits"].(map[string]any)
	if traits["label"] != "before" || traits["binary"] != nil {
		t.Fatalf("unsafe snapshot: %v", traits)
	}
}

func TestManualSpanParentGetterFailureStillRunsWork(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	old := hook.Tracing.ParentContext
	t.Cleanup(func() { hook.Tracing.ParentContext = old })
	hook.Tracing.ParentContext = func(context.Context) (string, string, string) { panic("parent getter") }
	_, span := StartSpan(context.Background(), SpanOptions{})
	span.End(nil)
}

func TestStandaloneManualSpanStartIdentityAndFallback(t *testing.T) {
	for _, user := range []string{"", "alice"} {
		t.Run(user, func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(true))
			ctx := WithValues(context.Background(), ContextValues{UserID: user})
			_, span := StartSpan(ctx, SpanOptions{})
			_ = WithValues(ctx, ContextValues{UserID: "bob"})
			span.End(nil)
			event := r.named("ai_otel_span")[0]
			if event.UserId != user || (user == "" && event.AnonymousId != systemAnonymousID) {
				t.Fatalf("identity=%q/%q", event.UserId, event.AnonymousId)
			}
		})
	}
}

func TestSpanDocsDescribeEmittedCallID(t *testing.T) {
	// The event join itself is asserted by the wrapper conformance tests.
	doc, err := os.ReadFile("docs/events.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "span_id` is not sent yet") {
		t.Fatal("events documentation contradicts emitted span_id")
	}
}

func TestSameInteractionCreditsCallbackCannotClaimWrapperSpan(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
	ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	defer owner.End(EndOptions{})
	_, span := beginWrapperSpan(ctx, owner, time.Now())
	reentered := false
	if err := Configure(WithCredits(func(CreditCall) (*float64, error) {
		if !reentered {
			reentered = true
			owner.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "auxiliary", InputTokens: 1})
			if len(r.named("ai_otel_span")) != 0 {
				t.Fatal("callback finished primary span")
			}
		}
		return Ptr(1.0), nil
	})); err != nil {
		t.Fatal(err)
	}
	owner.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "primary", InputTokens: 2}, span, 200)
	owner.End(EndOptions{})
	if spans := r.named("ai_otel_span"); len(spans) != 1 || spans[0].Properties["span_id"] != span.SpanID {
		t.Fatal("primary did not finish once")
	}
	for _, event := range r.named("ai_llm_response_received") {
		id := event.Properties["span_id"]
		if event.Properties["response_id"] == "auxiliary" && id != nil {
			t.Fatal("auxiliary claimed wrapper span")
		}
		if event.Properties["response_id"] == "primary" && id != span.SpanID {
			t.Fatal("primary lost wrapper span")
		}
	}
}

func TestSpanNormalizationFailureIsContained(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		t.Run(testBoolName(wrapper), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
			reports := 0
			if err := Configure(WithOnError(func(error, ErrorInfo) { reports++ })); err != nil {
				t.Fatal(err)
			}
			old := hook.Tracing.NormalizeValue
			t.Cleanup(func() { hook.Tracing.NormalizeValue = old })
			hook.Tracing.NormalizeValue = func(any) (any, bool) { panic("normalize") }
			called := 0
			if wrapper {
				ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				defer owner.End(EndOptions{})
				_, span := beginWrapperSpan(ctx, owner, time.Now())
				called++
				if !owner.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1}, span, 200) {
					t.Fatal("span failure undid call admission")
				}
				owner.End(EndOptions{})
				if len(r.named("ai_llm_response_received")) != 1 {
					t.Fatal("span failure lost product call")
				}
			} else {
				_, span := StartSpan(context.Background(), SpanOptions{})
				called++
				span.End(nil)
				span.End(nil)
			}
			if called != 1 || reports != 1 || len(r.named("ai_otel_span")) != 0 {
				t.Fatalf("work=%d reports=%d", called, reports)
			}
		})
	}
}
