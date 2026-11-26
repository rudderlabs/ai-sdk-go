package rudderstackai

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPendingCallEventsSurvivePauseRestore(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, unknown := range []bool{false, true} {
			for _, twin := range []bool{false, true} {
				t.Run(fmt.Sprintf("restore=%t/unknown=%t/twin=%t", restore, unknown, twin), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true))
					client := &pauseReentrantClient{recorder: r}
					_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
					call := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10, UsageUnknown: unknown}
					if unknown {
						call.Integration = "openai"
					}
					client.reenter = func() { in.RecordCall(call) }
					snapshot, ok := in.Pause(PauseOptions{})
					if !ok || len(snapshot.PendingCalls) != 1 {
						t.Fatal("pause did not carry the pending call", snapshot)
					}
					auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 0})
					if len(r.named("ai_llm_response_received")) != 0 {
						t.Fatal("provisional call emitted during pause delivery")
					}
					in = round3Resume(t, in, snapshot, restore)
					wantTokens := 10
					if twin {
						call.Integration, call.UsageUnknown, call.InputTokens = "openai", false, 20
						if unknown {
							call.Integration = "tracing"
						}
						in.RecordCall(call)
						wantTokens = 20
					}
					in.End(EndOptions{})
					auditAssertSubset(t, lastResponse(t, r), map[string]any{"llm_call_count": 1, "total_input_tokens": wantTokens})
					events := r.named("ai_llm_response_received")
					if len(events) != 1 {
						t.Fatalf("call events = %d, want 1", len(events))
					}
					auditAssertSubset(t, events[0].Properties, map[string]any{"input_tokens": wantTokens})
					in.End(EndOptions{})
					if len(r.named("ai_llm_response_received")) != 1 {
						t.Fatal("call event replayed")
					}
				})
			}
		}
	}
}

func TestCallMatchingRejectsDifferentAgents(t *testing.T) {
	for _, spanFirst := range []bool{false, true} {
		for _, counts := range [][2]int{{1, 1}, {3, 1}} {
			t.Run(fmt.Sprintf("spanFirst=%t/counts=%v", spanFirst, counts), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) { return Ptr(float64(c.CallCount)), nil }))
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				span := LLMCall{Provider: "openai", Model: "gpt-4.1", AgentName: "a", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10, CallCount: counts[0]}
				wrapper := span
				wrapper.AgentName, wrapper.Integration, wrapper.InputTokens, wrapper.CallCount = "b", "openai", 20, counts[1]
				if spanFirst {
					in.RecordCall(span)
					in.RecordCall(wrapper)
				} else {
					in.RecordCall(wrapper)
					in.RecordCall(span)
				}
				in.End(EndOptions{})
				wantCount := counts[0] + counts[1]
				auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": wantCount, "total_input_tokens": 30, "total_credits": float64(wantCount)})
				if len(r.named("ai_llm_response_received")) != 2 {
					t.Fatal("different agents lost a call event")
				}
				rows := r.response(t)["usage_breakdown"].([]map[string]any)
				if len(rows) != 2 {
					t.Fatal("different agents lost usage attribution", rows)
				}
				for _, row := range rows {
					count, tokens := counts[0], 10
					if row["agent_name"] == "b" {
						count, tokens = counts[1], 20
					}
					auditAssertSubset(t, row, map[string]any{"call_count": count, "input_tokens": tokens, "credits": float64(count)})
				}
			})
		}
	}
}
