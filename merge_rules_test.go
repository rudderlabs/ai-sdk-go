package rudderstackai

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMergeOutcomeFollowsReportOrder(t *testing.T) {
	for _, appError := range []bool{false, true} {
		t.Run(fmt.Sprint(appError), func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			a := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
			in.RecordCall(a)
			b := a
			b.StartedAt, b.EndedAt, b.ErrorType = time.UnixMilli(200), time.UnixMilli(210), "timeout"
			in.RecordCall(b)
			a.Integration = "tracing"
			in.RecordCall(a)
			if in.successfulCallSequence != 1 {
				t.Errorf("successful sequence = %d, want 1", in.successfulCallSequence)
			}
			o := EndOptions{}
			if appError {
				o.Status, o.ErrorType = StatusError, "app_error"
			}
			in.End(o)
			auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 2, "response_status": "timeout", "error_type": "timeout"})
		})
	}
}

func TestMergeKeepsAggregateCallCount(t *testing.T) {
	for _, counts := range [][2]int{{1, 3}, {3, 1}} {
		for _, spanFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("span=%d/wrapper=%d/spanFirst=%t", counts[0], counts[1], spanFirst), func(t *testing.T) {
				r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) { return Ptr(float64(c.CallCount)), nil }))
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				span := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), CallCount: counts[0], InputTokens: 10}
				wrapper := span
				wrapper.Integration, wrapper.CallCount, wrapper.InputTokens = "openai", counts[1], 30
				if spanFirst {
					in.RecordCall(span)
					in.RecordCall(wrapper)
				} else {
					in.RecordCall(wrapper)
					in.RecordCall(span)
				}
				in.End(EndOptions{})
				auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 3, "total_input_tokens": 30, "total_credits": 3.0})
				breakdown := r.response(t)["usage_breakdown"].([]map[string]any)
				auditAssertSubset(t, breakdown[0], map[string]any{"call_count": 3})
			})
		}
	}
}

func TestMergeKeepsKnownFinishReason(t *testing.T) {
	for _, reason := range []string{"max_tokens", "length", "content_filter"} {
		for _, later := range []string{"", "stop"} {
			t.Run(reason+"/"+later, func(t *testing.T) {
				r := setupAI(t)
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				call := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), FinishReason: reason}
				in.RecordCall(call)
				call.Integration, call.FinishReason = "tracing", later
				in.RecordCall(call)
				in.End(EndOptions{})
				wantReason := reason
				if reason == "content_filter" && later != "" {
					wantReason = later
				}
				auditAssertSubset(t, r.response(t), map[string]any{"finish_reason": wantReason})
				if reason != "content_filter" {
					auditAssertSubset(t, r.response(t), map[string]any{"response_status": "incomplete", "error_type": "length"})
				}
			})
		}
	}
}

func TestOverflowWaitsForPendingCalls(t *testing.T) {
	for _, flush := range []string{"merge", "end", "pause"} {
		t.Run(flush, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			a := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10}
			in.RecordCall(a)
			b := a
			b.Integration, b.StartedAt, b.EndedAt, b.InputTokens = "openai", time.UnixMilli(200), time.UnixMilli(210), 20
			in.RecordCall(b)
			b.StartedAt, b.EndedAt, b.InputTokens = time.UnixMilli(300), time.UnixMilli(310), 30
			in.RecordCall(b)
			if got := len(r.named("ai_llm_response_received")); got != 0 {
				t.Errorf("%d overflow events passed the pending call", got)
			}
			switch flush {
			case "merge":
				a.Integration = "openai"
				in.RecordCall(a)
			case "end":
				in.End(EndOptions{})
			case "pause":
				if _, ok := in.Pause(PauseOptions{}); !ok {
					t.Fatal("pause failed")
				}
			}
			events := r.named("ai_llm_response_received")
			if len(events) != 2 {
				t.Fatalf("got %d events, want deferred call and overflow aggregate", len(events))
			}
			auditAssertSubset(t, events[0].Properties, map[string]any{"input_tokens": 10})
			auditAssertSubset(t, events[1].Properties, map[string]any{"input_tokens": 50})
			in.End(EndOptions{})
			if len(r.named("ai_llm_response_received")) != 2 {
				t.Fatal("overflow emitted twice")
			}
		})
	}
}
