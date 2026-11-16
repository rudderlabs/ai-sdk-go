package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
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
func TestSDK6942AuthoritativeTwinsReprice(t *testing.T) {
	for _, id := range []string{"", "r"} {
		for _, unknown := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(id+map[bool]string{false: "known", true: "unknown"}[unknown]+map[bool]string{false: "hook", true: "explicit"}[explicit], func(t *testing.T) {
					priced := []int{}
					r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) {
						priced = append(priced, c.InputTokens)
						return Ptr(float64(c.InputTokens)), nil
					}))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					c := sdk6942Call()
					c.ResponseID = id
					c.InputTokens = 3
					c.UsageUnknown = unknown
					if explicit {
						c.Credits = Ptr(7.0)
					}
					i.RecordCall(c)
					c.Integration = "openai"
					c.InputTokens = 10
					c.UsageUnknown = false
					c.Credits = nil
					i.RecordCall(c)
					i.End(EndOptions{})
					if lastResponse(t, r)["total_credits"] != 10.0 {
						t.Fatal(lastResponse(t, r))
					}
					if len(priced) == 0 || priced[len(priced)-1] != 10 {
						t.Fatal(priced)
					}
				})
			}
		}
	}
}
func TestSDK6942AggregatePendingSnapshotsRankReports(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cross struct {
			Order struct {
				Snapshots []struct {
					Writer   string
					Snapshot PausedInteraction
				}
				Expected map[string]any
			} `json:"aggregate_report_order"`
		} `json:"cross_sdk"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, entry := range fixture.Cross.Order.Snapshots {
		t.Run(entry.Writer, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &entry.Snapshot})
			c := sdk6942Call()
			c.Integration = "openai"
			c.Model = "gpt-4.1"
			c.ResponseID = "second"
			i.RecordCall(sdk6111Window(c, 200))
			c.Model = "gpt-4o-mini"
			c.ResponseID = "third"
			c.InputTokens = 20
			i.RecordCall(sdk6111Window(c, 300))
			i.End(EndOptions{})
			p := lastResponse(t, r)
			for k, want := range fixture.Cross.Order.Expected {
				if n, ok := want.(float64); ok {
					if sdk6111Num(p[k]) != n {
						t.Fatal(p)
					}
				} else if p[k] != want {
					t.Fatal(p)
				}
			}
		})
	}
}
