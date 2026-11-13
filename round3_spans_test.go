package rudderstackai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestReentrantCreditsKeepsInvocationSpan(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
	ctx, alice := StartInteraction(context.Background(), InteractionOptions{UserID: "alice", Classify: Ptr(false)})
	ctx, span := beginWrapperSpan(ctx, alice, time.Now())
	bobCtx, bob := StartInteraction(ctx, InteractionOptions{UserID: "bob", Classify: Ptr(false)})
	defer alice.End(EndOptions{})
	defer bob.End(EndOptions{})
	reentered := false
	if err := Configure(WithCredits(func(CreditCall) (*float64, error) {
		if !reentered {
			reentered = true
			bob.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "bob-manual", InputTokens: 1})
			_, own := beginWrapperSpan(bobCtx, bob, time.Now())
			bob.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "bob-wrapper", InputTokens: 1}, own, 200)
		}
		return Ptr(1.0), nil
	})); err != nil {
		t.Fatal(err)
	}
	alice.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "alice", InputTokens: 1}, span, 200)
	alice.End(EndOptions{})
	bob.End(EndOptions{})
	if len(r.named("ai_llm_response_received")) != 3 {
		t.Fatal("reentrant calls lost")
	}
	spans := r.named("ai_otel_span")
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	for _, call := range r.named("ai_llm_response_received") {
		if call.Properties["response_id"] == "bob-manual" && call.Properties["span_id"] != nil {
			t.Fatal("manual record stole span")
		}
		if id := call.Properties["span_id"]; id != nil {
			matched := false
			for _, s := range spans {
				if s.Properties["span_id"] == id && s.Properties["interaction_id"] == call.Properties["interaction_id"] && s.UserId == call.UserId {
					matched = true
				}
			}
			if !matched {
				t.Fatal("call joined foreign span")
			}
		}
	}
	if spans[0].Properties["parent_span_id"] != nil {
		t.Fatal("reentrant interaction inherited wrapper")
	}
}

func TestStandaloneSpanCapturesStartContext(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	other := &recorder{}
	traits := map[string]any{"plan": "start"}
	ctx := WithValues(context.Background(), ContextValues{UserID: "alice", AnonymousID: "visitor", Analytics: r, Context: map[string]any{"traits": traits}})
	_, span := StartSpan(ctx, SpanOptions{Name: "standalone", Operation: "retrieval"})
	traits["plan"] = "end"
	if err := Configure(WithAnalytics(other)); err != nil {
		t.Fatal(err)
	}
	bobCtx := WithValues(ctx, ContextValues{UserID: "bob", Analytics: other})
	_, bob := StartInteraction(bobCtx, InteractionOptions{UserID: "bob", Classify: Ptr(false)})
	defer bob.End(EndOptions{})
	span.End(nil)
	if len(other.named("ai_otel_span")) != 0 {
		t.Fatal("end client stole span")
	}
	events := r.named("ai_otel_span")
	if len(events) != 1 || events[0].UserId != "alice" || events[0].AnonymousId != "visitor" {
		t.Fatalf("start identity lost: %v", events)
	}
	if events[0].Context.Extra["traits"] == nil || events[0].Context.Extra["traits"].(map[string]any)["plan"] != "start" {
		t.Fatal("start traits lost or mutated")
	}
}

func TestSDKSpanIdentityMatchesProductEventsUnderOTel(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
	previousParent := hook.Tracing.ParentContext
	t.Cleanup(func() { hook.Tracing.ParentContext = previousParent })
	hook.Tracing.ParentContext = func(context.Context) (string, string, string) {
		return strings.Repeat("a", 32), strings.Repeat("b", 16), ""
	}
	for _, user := range []string{"alice", "bob", ""} {
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: user, Trigger: TriggerSystem, Classify: Ptr(false)})
		_, manual := StartSpan(ctx, SpanOptions{Name: "manual"})
		manual.End(nil)
		_, wrapper := beginWrapperSpan(ctx, i, time.Now())
		i.recordWrapperCall(LLMCall{Model: "m", ResponseID: i.ID()}, wrapper, 200)
		i.End(EndOptions{})
		product := r.named("ai_llm_response_received")
		want := product[len(product)-1]
		spans := r.named("ai_otel_span")
		for _, s := range spans[len(spans)-2:] {
			if s.UserId != want.UserId || s.AnonymousId != want.AnonymousId || s.AnonymousId == manual.TraceID {
				t.Fatalf("SDK identity differs from product: span=%q/%q product=%q/%q", s.UserId, s.AnonymousId, want.UserId, want.AnonymousId)
			}
		}
	}
	_, s := StartSpan(context.Background(), SpanOptions{Name: "no identity"})
	s.End(nil)
	spans := r.named("ai_otel_span")
	if spans[len(spans)-1].AnonymousId != systemAnonymousID {
		t.Fatal("standalone identity differs from product fallback")
	}
}

func TestInteractionTraceFixedAcrossOTelAndResume(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(fmt.Sprint(inherited), func(t *testing.T) {
			setupAI(t, WithEmitSpans(true))
			trace := ""
			if inherited {
				trace = strings.Repeat("a", 32)
			}
			previousParent := hook.Tracing.ParentContext
			t.Cleanup(func() { hook.Tracing.ParentContext = previousParent })
			hook.Tracing.ParentContext = func(context.Context) (string, string, string) { return trace, strings.Repeat("b", 16), "vendor=value" }
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "alice", Classify: Ptr(false)})
			defer i.End(EndOptions{})
			_, first := StartSpan(ctx, SpanOptions{Name: "first"})
			first.End(nil)
			if i.sdkTraceID != first.TraceID {
				t.Fatal("first trace was not stored immediately")
			}
			snapshot, ok := i.Pause(PauseOptions{Reason: "approval"})
			if !ok || snapshot.SDKTraceID != first.TraceID {
				t.Fatal("pause lost trace")
			}
			trace = strings.Repeat("c", 32)
			_, second := beginWrapperSpan(ctx, i, time.Now())
			second.End(nil)
			if second.TraceID != first.TraceID || second.parentID != "" {
				t.Fatal("later external parent split interaction trace")
			}
			ctx, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot, Classify: Ptr(false)})
			defer resumed.End(EndOptions{})
			_, third := StartSpan(ctx, SpanOptions{Name: "resumed"})
			third.End(nil)
			if third.TraceID != first.TraceID || third.parentID != "" {
				t.Fatal("resume inherited foreign trace")
			}
			trace = first.TraceID
			_, same := StartSpan(ctx, SpanOptions{Name: "same trace"})
			same.End(nil)
			if same.TraceID != first.TraceID || same.parentID != strings.Repeat("b", 16) {
				t.Fatal("matching external parent ignored")
			}
		})
	}
}

func TestWrapperExceptionAlwaysRedacted(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, drop := range []bool{false, true} {
			t.Run(fmt.Sprintf("capture=%v/drop=%v", capture, drop), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(true), WithCaptureContent(capture), WithRedactHook(func(s string) (string, error) {
					if drop {
						return "", errors.New("drop")
					}
					return strings.ReplaceAll(s, "SECRET", "safe"), nil
				}))
				ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "alice", Classify: Ptr(false)})
				defer i.End(EndOptions{})
				_, span := beginWrapperSpan(ctx, i, time.Now())
				hook.Tracing.FinishRaw(span, "openai", "m", 500, time.Now(), errors.New("SECRET person@example.com"))
				encoded := mustJSON(r.named("ai_otel_span")[0])
				if strings.Contains(encoded, "SECRET") || strings.Contains(encoded, "person@example.com") {
					t.Fatalf("error leaked: %s", encoded)
				}
				if strings.Contains(encoded, "exception.message") != (capture && !drop) {
					t.Fatal("exception capture policy wrong")
				}
			})
		}
	}
}

func TestManualSpanSummaryNeverContainsBinary(t *testing.T) {
	for _, emit := range []bool{false, true} {
		for _, capture := range []bool{false, true} {
			t.Run(fmt.Sprintf("emit=%v/capture=%v", emit, capture), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(emit), WithCaptureContent(capture))
				ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "alice", Classify: Ptr(false)})
				_, s := StartSpan(ctx, SpanOptions{Name: "manual", Attributes: map[string]any{"gen_ai.response.finish_reasons": []any{"stop", []byte("PRIVATE")}, "input.value": map[string]any{"bytes": []byte("PRIVATE"), "text": "keep"}}})
				s.End(nil)
				i.End(EndOptions{})
				if strings.Contains(mustJSON(r.all()), "UFJJVkFURQ==") {
					t.Fatal("binary leaked")
				}
				want := 0
				if emit {
					want = 1
				}
				if len(r.named("ai_otel_span")) != want {
					t.Fatal("emit policy wrong")
				}
			})
		}
	}
}
