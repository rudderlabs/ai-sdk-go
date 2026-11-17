package rudderstackai

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// No private configuration or tracing hooks: the public reports must enforce billing themselves.
func TestFailedTwinKeepsOnlyProviderAmounts(t *testing.T) {
	for _, failure := range []string{"api_error", "timeout", "rate_limit", "cancelled"} {
		for _, spanFirst := range []bool{false, true} {
			for _, explicit := range []string{"", "wrapper", "span"} {
				for _, known := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/span-first=%t/explicit=%s/known=%t", failure, spanFirst, explicit, known), func(t *testing.T) {
						r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { return Ptr(7.0), nil }))
						mustPrice(t, "gpt-4o", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1000000}})
						_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
						common := LLMCall{Provider: "openai", Model: "gpt-4o", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
						wrapper, span := common, common
						wrapper.Integration, wrapper.ErrorType, wrapper.UsageUnknown = "openai", failure, !known
						if known {
							wrapper.InputTokens = 10
						}
						span.Integration, span.InputTokens, span.OutputTokens = "tracing", 10, 2
						switch explicit {
						case "wrapper":
							wrapper.CostUSD, wrapper.Credits = Ptr(3.0), Ptr(5.0)
						case "span":
							span.CostUSD, span.Credits = Ptr(3.0), Ptr(5.0)
						}
						if spanFirst {
							i.RecordCall(span)
							i.RecordCall(wrapper)
						} else {
							i.RecordCall(wrapper)
							i.RecordCall(span)
						}
						i.End(EndOptions{})
						response, call := r.props(t, "ai_response_received"), r.props(t, "ai_llm_response_received")
						if response["error_type"] != failure || call["error_type"] != failure || sdk6111Num(response["llm_call_count"]) != 1 || sdk6111Num(response["total_input_tokens"]) != 10 {
							t.Fatal(response, call)
						}
						if explicit == "" {
							for _, key := range []string{"total_cost_usd", "total_credits"} {
								if response[key] != nil {
									t.Fatal("derived billing", response)
								}
							}
							if call["cost_usd"] != nil || call["credits"] != nil {
								t.Fatal("derived call billing", call)
							}
						} else {
							if sdk6111Num(response["total_cost_usd"]) != 3 || sdk6111Num(response["total_credits"]) != 5 {
								t.Fatal("provider amounts lost", response)
							}
							// A known wrapper can already have emitted before the span enriches totals.
							if (explicit != "span" || !known || spanFirst) && (sdk6111Num(call["cost_usd"]) != 3 || sdk6111Num(call["credits"]) != 5) {
								t.Fatal("provider call amounts lost", call)
							}
						}
					})
				}
			}
		}
	}
}

func TestFailedTwinPricingProvenanceSurvivesJSON(t *testing.T) {
	for _, failure := range []string{"api_error", "timeout", "rate_limit", "cancelled"} {
		for _, spanFirst := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/span-first=%t/explicit=%t", failure, spanFirst, explicit), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { return Ptr(7.0), nil }))
					mustPrice(t, "gpt-4o", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1000000}})
					span := LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "tracing", InputTokens: 10, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
					wrapper := span
					wrapper.Integration, wrapper.ErrorType, wrapper.UsageUnknown, wrapper.InputTokens = "openai", failure, true, 0
					if explicit {
						span.CostUSD, span.Credits = Ptr(3.0), Ptr(5.0)
					}
					first, second := wrapper, span
					if spanFirst {
						first, second = span, wrapper
					}
					i := sdk6111ReviewPending(t, r, "json", func(i *Interaction) { i.RecordCall(first) })
					i.RecordCall(second)
					i.End(EndOptions{})
					response, call := lastResponse(t, r), r.props(t, "ai_llm_response_received")
					if response["error_type"] != failure || call["error_type"] != failure {
						t.Fatal(response, call)
					}
					if explicit {
						if sdk6111Num(response["total_cost_usd"]) != 3 || sdk6111Num(call["cost_usd"]) != 3 || sdk6111Num(response["total_credits"]) != 5 || sdk6111Num(call["credits"]) != 5 {
							t.Fatal("explicit amounts lost", response, call)
						}
					} else if response["total_cost_usd"] != nil || response["total_credits"] != nil || call["cost_usd"] != nil || call["credits"] != nil {
						t.Fatal("derived failure pricing", response, call)
					}
				})
			}
		}
	}
}
