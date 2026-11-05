package rudderstackai

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCallMatchingConsumesClosestReportOnce(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	report := func(source string, start, end int64) bool {
		return in.RecordCall(LLMCall{Provider: "anthropic", Model: "claude-opus-5", Integration: source, StartedAt: time.UnixMilli(start), EndedAt: time.UnixMilli(end), InputTokens: 1000, OutputTokens: 500})
	}
	if !report("anthropic", 100, 110) || !report("anthropic", 120, 130) {
		t.Fatal("separate wrapper calls were lost")
	}
	if report("tracing", 121, 129) {
		t.Fatal("closest span was counted twice")
	}
	if report("tracing", 101, 109) {
		t.Fatal("remaining wrapper was not available to match")
	}
	if !report("tracing", 101, 109) {
		t.Fatal("one wrapper matched more than one span")
	}
	in.End(EndOptions{})
	auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 3, "total_input_tokens": 3000, "total_output_tokens": 1500})
	if len(r.named("ai_llm_response_received")) != 3 {
		t.Fatal("unexpected call event count")
	}
}

func TestCallMatchingWrapperUsageWinsInBothOrders(t *testing.T) {
	for _, spanFirst := range []bool{false, true} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("spanFirst=%t/unknown=%t", spanFirst, unknown), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true))
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				span := LLMCall{Provider: "aws.bedrock", Model: "claude-opus-5", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10, OutputTokens: 5, UsageUnknown: unknown}
				wrapper := span
				wrapper.Model = "us.anthropic.claude-opus-5-20261101-v1:0"
				wrapper.Integration, wrapper.UsageUnknown = "anthropic", false
				wrapper.InputTokens, wrapper.OutputTokens = 1000, 500
				wrapper.CostUSD, wrapper.Credits = Ptr(0.004), Ptr(7.0)
				if spanFirst {
					in.RecordCall(span)
					in.RecordCall(wrapper)
				} else {
					in.RecordCall(wrapper)
					in.RecordCall(span)
				}
				in.End(EndOptions{})
				auditAssertSubset(t, r.response(t), map[string]any{"primary_model": "claude-opus-5", "llm_call_count": 1, "total_input_tokens": 1000, "total_output_tokens": 500, "total_cost_usd": 0.004, "total_credits": 7.0})
				if len(r.named("ai_llm_response_received")) != 1 {
					t.Fatal("duplicate call event")
				}
			})
		}
	}
}
