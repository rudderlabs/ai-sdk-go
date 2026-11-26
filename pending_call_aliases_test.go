package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

var review137Call = pendingPricingCall

func TestReview137InnerTwin(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			armed, nested := false, false
			priced := []string{}
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
				priced = append(priced, fmt.Sprintf("%d/%v/%v", c.InputTokens, c.BYOK != nil && *c.BYOK, c.FallbackUsed != nil && *c.FallbackUsed))
				if armed && !nested {
					nested = true
					cc := review137Call()
					cc.Integration = "openai"
					cc.InputTokens = 1000
					i.RecordCall(cc)
					cc = review137Call()
					cc.BYOK = Ptr(true)
					cc.FallbackUsed = Ptr(true)
					i.RecordCall(cc)
				}
				v := float64(c.InputTokens)
				if c.BYOK != nil && *c.BYOK {
					v *= 2
				}
				return Ptr(v), nil
			}))
			c := review137Call()
			c.BYOK = Ptr(false)
			c.FallbackUsed = Ptr(false)
			i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(c) })
			armed = true
			c.Integration = "openai"
			c.InputTokens = 100
			i.RecordCall(c)
			i.End(EndOptions{})
			p := lastResponse(t, r)
			fmt.Printf("INNER_TWIN_RESULT priced=%v response=%#v calls=%#v\n", priced, p, r.named("ai_llm_response_received"))
			if p["total_credits"] != 2000.0 {
				t.Fatal(p)
			}
			if r.named("ai_llm_response_received")[0].Properties["byok"] != true {
				t.Fatal("stale byok")
			}
		})
	}
}

func TestReview137CanonicalOutcome(t *testing.T) {
	for _, mode := range []string{"in-process", "json"} {
		for _, key := range []string{"errorType", "pauseReason"} {
			t.Run(mode+"/"+key, func(t *testing.T) {
				r := setupAI(t)
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				s, _ := i.Pause(PauseOptions{})
				status := "paused"
				if key == "errorType" {
					status = "error"
				}
				s.OutcomeHint = map[string]any{"status": status, "calls": 0, "errorType": nil, "pauseReason": nil, "error_type": "api_error", "pause_reason": "approval"}
				if mode == "json" {
					raw, err := json.Marshal(s)
					if err != nil {
						t.Fatal(err)
					}
					if err = json.Unmarshal(raw, &s); err != nil {
						t.Fatal(err)
					}
				}
				_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
				restored.End(EndOptions{})
				p := lastResponse(t, r)
				if restored.ID() != i.ID() {
					t.Fatal("lost identity")
				}
				if key == "errorType" && p["error_type"] != nil {
					t.Fatal(p)
				}
				if key == "pauseReason" && p["pause_reason"] != "other" {
					t.Fatal(p)
				}
			})
		}
	}
}

func TestReview137CaptureEnrichment(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			armed := false
			r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithCredits(func(c CreditCall) (*float64, error) {
				value := float64(c.InputTokens)
				if c.BYOK != nil && *c.BYOK {
					value *= 2
				}
				return Ptr(value), nil
			}), WithRedactHook(func(text string) (string, error) {
				if armed && text == "older" {
					armed = false
					c := review137Call()
					c.BYOK = Ptr(true)
					c.FallbackUsed = Ptr(true)
					i.RecordCall(c)
				}
				return text, nil
			}))
			c := review137Call()
			c.BYOK = Ptr(false)
			c.FallbackUsed = Ptr(false)
			i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(c) })
			armed = true
			c.Integration = "openai"
			c.InputTokens = 1000
			c.OutputText = "older"
			c.BYOK = nil
			c.FallbackUsed = nil
			i.RecordCall(c)
			i.End(EndOptions{})
			p := r.named("ai_llm_response_received")[0].Properties
			if p["input_tokens"] != 1000 || p["byok"] != true || p["fallback_used"] != true || p["credits"] != 2000.0 {
				t.Fatal(p)
			}
			if p := lastResponse(t, r); p["total_credits"] != 2000.0 {
				t.Fatal(p)
			}
		})
	}
}

func TestCaptureBeforeThirdPricingAttempt(t *testing.T) {
	for _, flag := range []string{"byok", "fallback_used"} {
		for _, source := range []string{"redactor", "marshaler"} {
			t.Run(flag+"/"+source, func(t *testing.T) {
				var i *Interaction
				armed, attempts := false, 0
				var notices []string
				call := review137Call()
				setFlag := func(c *LLMCall, value bool) {
					if flag == "byok" {
						c.BYOK = Ptr(value)
					} else {
						c.FallbackUsed = Ptr(value)
					}
				}
				enrich := func() {
					if armed {
						c := call
						setFlag(&c, true)
						i.RecordCall(c)
					}
				}
				r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true),
					WithOnError(func(_ error, info ErrorInfo) { notices = append(notices, info.Kind) }),
					WithCredits(func(b CreditCall) (*float64, error) {
						value := b.BYOK
						if flag == "fallback_used" {
							value = b.FallbackUsed
						}
						enabled := value != nil && *value
						if armed && b.InputTokens == 100 {
							attempts++
							if attempts < 3 {
								c := call
								setFlag(&c, !enabled)
								i.RecordCall(c)
							}
						}
						credits := float64(b.InputTokens)
						if enabled {
							credits *= 2
						}
						return Ptr(credits), nil
					}), WithRedactHook(func(text string) (string, error) {
						if source == "redactor" && text == "late-change" {
							enrich()
						}
						return text, nil
					}))
				_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				c := call
				setFlag(&c, false)
				i.RecordCall(c)
				armed = true
				c = call
				c.Integration = "openai"
				c.InputTokens = 100
				c.OutputText = "late-change"
				if source == "marshaler" {
					c.InputMessages = []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": callbackContent{enrich}}}}}
				}
				i.RecordCall(c)
				armed = false
				i.End(EndOptions{})
				p := r.named("ai_llm_response_received")[0].Properties
				if attempts != 3 || len(notices) != 0 || p[flag] != true || p["credits"] != 200.0 {
					t.Fatalf("attempts=%d notices=%v call=%v", attempts, notices, p)
				}
				if lastResponse(t, r)["total_credits"] != 200.0 {
					t.Fatal(lastResponse(t, r))
				}
			})
		}
	}
}
