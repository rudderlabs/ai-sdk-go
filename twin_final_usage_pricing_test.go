package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFailedSpanSuccessfulWrapperPricesFinalUsage(t *testing.T) {
	for _, terminal := range []string{"api_error", "cancelled", "timeout"} {
		for _, responseID := range []string{"r", ""} {
			t.Run(fmt.Sprintf("%s/responseID=%s", terminal, responseID), func(t *testing.T) {
				var priced []CreditCall
				r := setupAI(t, WithLLMCallEvents(true), WithPrices(map[string]ModelPrice{
					"gpt-4o": {TokenPrice: TokenPrice{InputPerMtok: 1000000}},
				}), WithCredits(func(c CreditCall) (*float64, error) {
					priced = append(priced, c)
					return Ptr(float64(c.InputTokens)), nil
				}))
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: responseID, Integration: "tracing", InputTokens: 10, ErrorType: terminal})
				i.RecordCall(span)
				wrapper := span
				wrapper.Integration, wrapper.InputTokens, wrapper.ErrorType = "openai", 100, ""
				i.RecordCall(wrapper)
				i.End(EndOptions{})
				if len(priced) != 1 || priced[0].InputTokens != 100 || priced[0].CostUSD == nil || *priced[0].CostUSD != 100 {
					t.Errorf("successful wrapper pricing basis: %+v", priced)
				}
				auditAssertSubset(t, r.response(t), map[string]any{"response_status": "success", "llm_call_count": 1, "total_input_tokens": 100, "total_cost_usd": 100.0, "total_credits": 100.0})
				events := r.named("ai_llm_response_received")
				if len(events) != 1 {
					t.Fatalf("call events = %d, want 1", len(events))
				}
				auditAssertSubset(t, events[0].Properties, map[string]any{"input_tokens": 100, "cost_usd": 100.0, "credits": 100.0})
			})
		}
	}
}

func TestTwinAmountsFollowKeptUsage(t *testing.T) {
	for _, models := range [][2]string{{"gpt-4o-2024-08-06", "gpt-4o"}, {"priced-span", "unpriced-wrapper"}} {
		for _, spanFirst := range []bool{true, false} {
			for _, priceWrapper := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/spanFirst=%t/priceWrapper=%t", models[0], spanFirst, priceWrapper), func(t *testing.T) {
					prices := map[string]ModelPrice{models[0]: {TokenPrice: TokenPrice{InputPerMtok: 1000000}}}
					if priceWrapper {
						prices[models[1]] = ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 2000000}}
					}
					r := setupAI(t, WithLLMCallEvents(true), WithPrices(prices), WithCredits(func(c CreditCall) (*float64, error) {
						if c.CostUSD == nil {
							return nil, nil
						}
						return Ptr(*c.CostUSD * 2), nil
					}))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: models[0], Integration: "tracing", InputTokens: 10})
					if models[0] == "priced-span" {
						span.ResponseID = "same-call"
					}
					wrapper := span
					wrapper.Model, wrapper.Integration, wrapper.InputTokens = models[1], "openai", 100
					if spanFirst {
						i.RecordCall(span)
						i.RecordCall(wrapper)
					} else {
						i.RecordCall(wrapper)
						i.RecordCall(span)
					}
					i.End(EndOptions{})
					auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 1, "total_input_tokens": 100})
					rows := r.response(t)["usage_breakdown"].([]map[string]any)
					if len(rows) != 1 {
						t.Fatalf("usage rows = %d, want 1", len(rows))
					}
					if events := r.named("ai_llm_response_received"); len(events) != 1 {
						t.Fatalf("call events = %d, want 1", len(events))
					}
					for _, p := range []map[string]any{r.response(t), rows[0], r.named("ai_llm_response_received")[0].Properties} {
						costKey, creditsKey := "cost_usd", "credits"
						if _, ok := p["response_status"]; ok {
							costKey, creditsKey = "total_cost_usd", "total_credits"
						}
						if priceWrapper {
							auditAssertSubset(t, p, map[string]any{costKey: 200.0, creditsKey: 400.0})
						} else if p[costKey] != nil || p[creditsKey] != nil {
							t.Errorf("discarded span amounts survived: %v", p)
						}
					}
				})
			}
		}
	}
}

func TestFailedWrapperUnsetSpanPreservesFailure(t *testing.T) {
	for _, terminal := range []string{"cancelled", "api_error", "timeout"} {
		t.Run(terminal, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
				t.Error("failed wrapper must not invoke credits hook")
				return Ptr(1.0), nil
			}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			wrapper := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "unset", Integration: "openai", InputTokens: 10, ErrorType: terminal})
			i.RecordCall(wrapper)
			span := wrapper
			span.Integration, span.ErrorType = "tracing", ""
			i.RecordCall(span)
			i.End(EndOptions{})
			auditAssertSubset(t, r.response(t), map[string]any{"error_type": terminal, "llm_call_count": 1})
			if r.response(t)["total_credits"] != nil {
				t.Fatal("failed wrapper acquired credits")
			}
		})
	}
}

func TestFoldedFailureAmountsRequireSuccessfulReportsOnly(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "barrier", Integration: "tracing", Credits: Ptr(0.0), CostUSD: Ptr(0.0)}))
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 1, ErrorType: "api_error"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 2, CostUSD: Ptr(2.0), Credits: Ptr(3.0)})
	i.End(EndOptions{})
	events := r.named("ai_llm_response_received")
	if len(events) != 2 {
		t.Fatalf("call events = %d, want barrier and folded event", len(events))
	}
	p := events[1].Properties
	auditAssertSubset(t, p, map[string]any{"model": "gpt-4o", "input_tokens": 3, "cost_usd": 2.0, "credits": 3.0})
	if p["error_type"] != nil {
		t.Fatal("successful folded report must clear the error")
	}
}

func TestBytesInRootContainerStayOutOfCallContent(t *testing.T) {
	type Root struct{ Root []byte }
	for _, spans := range []bool{true, false} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithEmitSpans(spans))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: Root{[]byte("CUSTOMER_BINARY_SECRET")}}}})
			i.End(EndOptions{})
			wire, err := json.Marshal(r.named("ai_llm_response_received")[0].Properties)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "CUSTOMER_BINARY_SECRET") || strings.Contains(string(wire), "Q1VTVE9NRVJfQklOQVJZX1NFQ1JFVA==") {
				t.Fatalf("native bytes leaked into call content: %s", wire)
			}
		})
	}
}
