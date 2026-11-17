package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

func sdk6942Call() LLMCall {
	return sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "r", Integration: "tracing", InputTokens: 10}, 100)
}
func TestSDK6942BlockedRepeatPreservesAuthoritativeTwin(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, threaded := range []bool{false, true} {
			for _, same := range []bool{false, true} {
				t.Run(mode+map[bool]string{false: "reentrant", true: "blocked"}[threaded]+map[bool]string{false: "different", true: "same"}[same], func(t *testing.T) {
					var i *Interaction
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					wrapper := func() {
						c := sdk6942Call()
						c.Integration = "openai"
						c.InputTokens = 100
						c.CallPurpose = "answer"
						i.RecordCall(c)
					}
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
						if c.AgentName == "planner" && c.InputTokens == 10 {
							once.Do(func() {
								if threaded {
									close(entered)
									<-release
								} else {
									wrapper()
								}
							})
						}
						if same && c.InputTokens == 100 {
							return Ptr(10.0), nil
						}
						return Ptr(float64(c.InputTokens)), nil
					}))
					i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(sdk6942Call()) })
					repeat := sdk6942Call()
					repeat.AgentName = "planner"
					if threaded {
						done := make(chan struct{})
						go func() { defer close(done); i.RecordCall(repeat) }()
						select {
						case <-entered:
						case <-time.After(3 * time.Second):
							t.Fatal("hook not entered")
						}
						wrapper()
						close(release)
						select {
						case <-done:
						case <-time.After(3 * time.Second):
							t.Fatal("blocked hook")
						}
					} else {
						i.RecordCall(repeat)
					}
					i.End(EndOptions{})
					p := lastResponse(t, r)
					want := 100.0
					if same {
						want = 10
					}
					if p["total_credits"] != want || p["total_input_tokens"] != 100 {
						t.Fatal(p)
					}
					b := p["usage_breakdown"].([]map[string]any)[0]
					if b["call_purpose"] != "answer" || b["agent_name"] != "planner" {
						t.Fatal(b)
					}
				})
			}
		}
	}
}

func TestSDK6942RepeatFlagsUpdateHeldEvents(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, matched := range []bool{false, true} {
			for _, initial := range []bool{false, true} {
				for _, field := range []string{"byok", "fallback_used"} {
					t.Run(mode+field+map[bool]string{false: "false", true: "true"}[initial]+map[bool]string{false: "unmatched", true: "matched"}[matched], func(t *testing.T) {
						r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
							flag := c.BYOK
							if field == "fallback_used" {
								flag = c.FallbackUsed
							}
							if flag != nil && *flag {
								return Ptr(20.0), nil
							}
							return Ptr(10.0), nil
						}))
						c := sdk6942Call()
						if field == "byok" {
							c.BYOK = Ptr(initial)
						} else {
							c.FallbackUsed = Ptr(initial)
						}
						i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) {
							b := sdk6942Call()
							b.ResponseID = "barrier"
							b.Model = "barrier"
							b.InputTokens = 0
							b.Credits = Ptr(0.0)
							b.CostUSD = Ptr(0.0)
							i.RecordCall(sdk6111Window(b, 50))
							i.RecordCall(c)
							if matched {
								w := c
								w.Integration = "openai"
								i.RecordCall(w)
							}
						})
						if field == "byok" {
							c.BYOK = Ptr(!initial)
						} else {
							c.FallbackUsed = Ptr(!initial)
						}
						i.RecordCall(c)
						i.End(EndOptions{})
						p := lastResponse(t, r)
						want := 20.0
						if initial {
							want = 10
						}
						if p["total_credits"] != want {
							t.Fatal(p)
						}
						found := false
						for _, e := range r.named("ai_llm_response_received") {
							if e.Properties["response_id"] == "r" {
								found = true
								if e.Properties[field] != !initial {
									t.Fatal(e.Properties)
								}
							}
						}
						if !found {
							t.Fatal("missing held event")
						}
					})
				}
			}
		}
	}
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

func TestSDK6942PricingRevisionRejectsStaleEqualCredits(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			nested := false
			r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) {
				if c.InputTokens == 10 && c.AgentName == "planner" && !nested {
					nested = true
					w := sdk6942Call()
					w.Integration = "openai"
					w.InputTokens = 100
					i.RecordCall(w)
					return Ptr(10.0), nil
				}
				return Ptr(5.0), nil
			}))
			i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(sdk6942Call()) })
			c := sdk6942Call()
			c.AgentName = "planner"
			i.RecordCall(c)
			i.End(EndOptions{})
			p := lastResponse(t, r)
			if p["total_input_tokens"] != 100 || p["total_credits"] != 5.0 {
				t.Fatal(p)
			}
		})
	}
}
