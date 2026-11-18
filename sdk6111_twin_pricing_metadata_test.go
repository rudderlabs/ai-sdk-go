package rudderstackai

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func sdk6111TwinWindow(c LLMCall) LLMCall {
	c.StartedAt = time.Unix(100, 0)
	c.EndedAt = c.StartedAt.Add(10 * time.Millisecond)
	return c
}

type sdk6111TwinPauseClient struct {
	*recorder
	duringPause func()
}

func (r *sdk6111TwinPauseClient) Enqueue(m analytics.Message) error {
	err := r.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok && e.Event == "ai_response_received" && e.Properties["response_status"] == "paused" && r.duringPause != nil {
		r.duringPause()
	}
	return err
}
func sdk6111TwinPending(t *testing.T, r *recorder, mode string, call LLMCall) *Interaction {
	t.Helper()
	client := &sdk6111TwinPauseClient{recorder: r}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	if mode == "segment" {
		i.RecordCall(call)
		return i
	}
	client.duringPause = func() { i.RecordCall(call) }
	snapshot, ok := i.Pause(PauseOptions{})
	client.duringPause = nil
	if !ok {
		t.Fatal("pause failed")
	}
	if mode == "json" {
		wire, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var stored PausedInteraction
		if err = json.Unmarshal(wire, &stored); err != nil {
			t.Fatal(err)
		}
		_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	} else {
		i.Resume()
	}
	return i
}
func sdk6111TwinEnrich(c LLMCall, field string) LLMCall {
	switch field {
	case "call_purpose":
		c.CallPurpose = "answer"
	case "agent_name":
		c.AgentName = "answer"
	case "fallback_used":
		c.FallbackUsed = Ptr(true)
	case "byok":
		c.BYOK = Ptr(true)
	}
	return c
}
func sdk6111TwinFlag(c CreditCall, field string) bool {
	switch field {
	case "call_purpose":
		return c.CallPurpose == "answer"
	case "agent_name":
		return c.AgentName == "answer"
	case "fallback_used":
		return c.FallbackUsed != nil && *c.FallbackUsed
	case "byok":
		return c.BYOK != nil && *c.BYOK
	}
	return false
}
func TestSDK6111TwinUsesRetainedMetadata(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, field := range []string{"call_purpose", "agent_name", "fallback_used", "byok"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				var priced []CreditCall
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
					priced = append(priced, c)
					if sdk6111TwinFlag(c, field) {
						return Ptr(20.0), nil
					}
					return Ptr(10.0), nil
				}))
				span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "r", Integration: "tracing", InputTokens: 10})
				i := sdk6111TwinPending(t, r, mode, sdk6111TwinEnrich(span, field))
				wrapper := span
				wrapper.Integration = "openai"
				wrapper.InputTokens = 100
				i.RecordCall(wrapper)
				i.End(EndOptions{})
				p := lastResponse(t, r)
				t.Logf("pricing inputs=%+v; tokens=%v credits=%v breakdown=%v", priced, p["total_input_tokens"], p["total_credits"], p["usage_breakdown"])
				if p["total_input_tokens"] != 100 || p["total_credits"] != 20.0 {
					t.Fatalf("tokens=%v credits=%v; want 100 tokens and 20 credits for retained %s", p["total_input_tokens"], p["total_credits"], field)
				}
			})
		}
	}
}
