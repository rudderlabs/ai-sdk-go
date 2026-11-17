package rudderstackai

import (
	"fmt"
	"testing"
	"time"
)

func TestSDK6111TwinReentrantEnrichmentRepricesNewUsage(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, blocked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocked=%t", mode, blocked), func(t *testing.T) {
				var i *Interaction
				inject := false
				var priced []CreditCall
				entered, release := make(chan struct{}), make(chan struct{})
				span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "r", Integration: "tracing", InputTokens: 10})
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
					priced = append(priced, c)
					if inject {
						inject = false
						if blocked {
							close(entered)
							<-release
						} else {
							i.RecordCall(sdk6111TwinEnrich(span, "call_purpose"))
						}
					}
					if c.InputTokens == 10 && c.CallPurpose == "" {
						return nil, nil
					}
					return Ptr(float64(c.InputTokens)), nil
				}))
				i = sdk6111TwinPending(t, r, mode, span)
				wrapper := span
				wrapper.Integration = "openai"
				wrapper.InputTokens = 100
				inject = true
				if blocked {
					done := make(chan struct{})
					go func() { defer close(done); i.RecordCall(wrapper) }()
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("wrapper hook did not enter")
					}
					i.RecordCall(sdk6111TwinEnrich(span, "call_purpose"))
					close(release)
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("wrapper did not finish")
					}
				} else {
					i.RecordCall(wrapper)
				}
				i.End(EndOptions{})
				p := lastResponse(t, r)
				t.Logf("pricing inputs=%+v; tokens=%v credits=%v breakdown=%v", priced, p["total_input_tokens"], p["total_credits"], p["usage_breakdown"])
				if p["total_input_tokens"] != 100 || p["total_credits"] != 100.0 {
					t.Fatalf("tokens=%v credits=%v; want 100 tokens and 100 credits for authoritative usage", p["total_input_tokens"], p["total_credits"])
				}
			})
		}
	}
}
