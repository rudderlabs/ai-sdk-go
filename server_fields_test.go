package rudderstackai

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestCreditsHookAndOverflow(t *testing.T) {
	r := setupAI(t, WithCredits(func(call CreditCall) (*float64, error) { return Ptr(2.0), nil }), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "plan"})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "answer", Credits: Ptr(3.0)})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "answer", Credits: Ptr(4.0)})
	i.End(EndOptions{})
	if p := r.props(t, "ai_response_received"); p["total_credits"] != 9.0 {
		t.Fatalf("credits: %v", p)
	}
}

func TestCreditsUnknownNeverEmitPartialTotal(t *testing.T) {
	r := setupAI(t, WithCredits(func(call CreditCall) (*float64, error) {
		if call.Model == "unpriced" {
			return nil, nil
		}
		return Ptr(2.0), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	i.RecordCall(LLMCall{Model: "unpriced"})
	i.End(EndOptions{})
	if _, has := r.props(t, "ai_response_received")["total_credits"]; has {
		t.Fatal("partial credits emitted")
	}
}

func TestCreditHookSkipsFailedCancelledAndUnknown(t *testing.T) {
	calls := 0
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { calls++; return Ptr(5.0), nil }))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	i.RecordCall(LLMCall{Model: "m", ErrorType: "rate_limit"})
	i.RecordCall(LLMCall{Model: "m", ErrorType: "cancelled", Credits: Ptr(9.0)})
	i.RecordCall(LLMCall{Model: "m", UsageUnknown: true})
	i.End(EndOptions{})
	if calls != 1 {
		t.Fatalf("hook called %d times", calls)
	}
	if _, has := r.props(t, "ai_response_received")["total_credits"]; has {
		t.Fatal("partial credits emitted")
	}
}

func TestInvalidCredits(t *testing.T) {
	for _, amount := range []float64{-1, math.Inf(1), math.NaN()} {
		if creditsOf(&amount) != nil {
			t.Fatal("invalid credits accepted")
		}
	}
}

func TestCallTimingPreservesZeroAndFalse(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	now := time.Now()
	i.RecordCall(LLMCall{Model: "m", StartedAt: now, EndedAt: now, FirstTokenAt: now, FallbackUsed: Ptr(false), BYOK: Ptr(false)})
	i.End(EndOptions{})
	p := r.props(t, "ai_llm_response_received")
	if p["latency_ms"] != int64(0) || p["time_to_first_token_ms"] != int64(0) || p["fallback_used"] != false || p["byok"] != false {
		t.Fatalf("timing and flags: %v", p)
	}
}

func TestCreditHookCanInspectAndUpdateInteraction(t *testing.T) {
	var i *Interaction
	blocked := false
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) {
		done := make(chan struct{})
		go func() {
			_ = i.Ended()
			i.SetConfidence(ConfidenceHigh)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			blocked = true
		}
		return Ptr(2.0), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	if blocked {
		t.Fatal("credit hook could not call locking interaction methods")
	}
	i.End(EndOptions{})
	if p := r.props(t, "ai_response_received"); p["confidence"] != "high" || p["total_credits"] != 2.0 {
		t.Fatalf("hook changes: %v", p)
	}
}

func TestCallLatencyExcludesCreditHook(t *testing.T) {
	var hookStart time.Time
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
		hookStart = time.Now()
		time.Sleep(60 * time.Millisecond)
		return Ptr(2.0), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	started := time.Now()
	i.RecordCall(LLMCall{Model: "m", StartedAt: started})
	i.End(EndOptions{})
	latency := r.props(t, "ai_llm_response_received")["latency_ms"].(int64)
	if latency > hookStart.Sub(started).Milliseconds()+1 {
		t.Fatalf("provider latency %d ms includes credit hook", latency)
	}
}
