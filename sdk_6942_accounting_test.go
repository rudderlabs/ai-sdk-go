package rudderstackai

import (
	"context"
	"testing"
	"time"
)

func sdk6942Call() LLMCall {
	return sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "r", Integration: "tracing", InputTokens: 10}, 100)
}
func TestSDK6942CreditCallbackPointersAreIsolated(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
		if c.CallPurpose == "answer" {
			*c.CostUSD = 900
			*c.ReasoningTokens = 900
			*c.FallbackUsed = true
			*c.BYOK = true
		}
		return nil, nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	c := sdk6942Call()
	c.CostUSD = Ptr(0.01)
	c.OutputTokens = 10
	c.ReasoningTokens = Ptr(2)
	c.FallbackUsed = Ptr(false)
	c.BYOK = Ptr(false)
	i.RecordCall(c)
	c.CallPurpose = "answer"
	i.RecordCall(c)
	i.End(EndOptions{})
	p := lastResponse(t, r)
	if p["total_cost_usd"] != 0.01 || p["total_reasoning_tokens"] != 2 {
		t.Fatal(p)
	}
	b := r.named("ai_llm_response_received")[0].Properties
	if b["byok"] != false || b["fallback_used"] != false {
		t.Fatal(b)
	}
}
