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
					if len(r.named("ai_llm_response_received")) != 1 {
						t.Fatal("known wrapper event was delayed")
					}
					in.RecordCall(span)
				}
				in.End(EndOptions{})
				auditAssertSubset(t, r.response(t), map[string]any{"primary_model": "claude-opus-5", "llm_call_count": 1, "total_input_tokens": 1000, "total_output_tokens": 500, "total_cost_usd": 0.004, "total_credits": 7.0})
				if len(r.named("ai_llm_response_received")) != 1 {
					t.Fatal("duplicate call event")
				}
				auditAssertSubset(t, r.props(t, "ai_llm_response_received"), map[string]any{"input_tokens": 1000, "output_tokens": 500, "cost_usd": 0.004, "credits": 7.0})
			})
		}
	}
}

func TestCallMatchingLatestOutcomeWins(t *testing.T) {
	for _, tc := range []struct{ firstError, laterError, finishReason, status, errorType string }{
		{"api_error", "timeout", "", "timeout", "timeout"},
		{"timeout", "rate_limit", "", "error", "rate_limit"},
		{"timeout", "", "stop", "success", ""},
		{"", "timeout", "", "timeout", "timeout"},
		{"timeout", "", "length", "incomplete", "length"},
	} {
		for _, appError := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/%s/appError=%t", tc.firstError, tc.laterError, tc.finishReason, appError), func(t *testing.T) {
				r := setupAI(t)
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				span := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), ErrorType: tc.firstError}
				wrapper := span
				wrapper.Integration, wrapper.ErrorType, wrapper.FinishReason = "openai", tc.laterError, tc.finishReason
				in.RecordCall(span)
				if in.RecordCall(wrapper) {
					t.Fatal("twin counted as a new call")
				}
				if in.lastCall.errorType != tc.laterError || (in.lastOK != nil) != (tc.laterError == "") {
					t.Error("merged outcome bookkeeping is stale")
				}
				o := EndOptions{}
				if appError {
					o.Status, o.ErrorType = StatusError, "app_error"
				}
				in.End(o)
				status, errorType := tc.status, tc.errorType
				if appError && errorType == "" {
					status, errorType = "error", "app_error"
				}
				p := r.response(t)
				auditAssertSubset(t, p, map[string]any{"llm_call_count": 1, "response_status": status})
				if got, _ := p["error_type"].(string); got != errorType {
					t.Fatalf("error_type = %q, want %q", got, errorType)
				}
			})
		}
	}
}

func TestCallMatchingUnmatchedEventsFlushAtSegmentEnd(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("pause=%t", pause), func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			in.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10, CostUSD: Ptr(0.004)})
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("published before a twin could enrich the report")
			}
			if pause {
				if _, ok := in.Pause(PauseOptions{}); !ok {
					t.Fatal("pause failed")
				}
			} else {
				in.End(EndOptions{})
			}
			auditAssertSubset(t, r.props(t, "ai_llm_response_received"), map[string]any{"input_tokens": 10, "cost_usd": 0.004})
			in.End(EndOptions{})
			if len(r.named("ai_llm_response_received")) != 1 {
				t.Fatal("segment finalization replayed the call event")
			}
		})
	}
}

func TestCallMatchingRegistersMergedResponseID(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	span := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputTokens: 10, UsageUnknown: true}
	wrapper := span
	wrapper.Integration, wrapper.ResponseID, wrapper.UsageUnknown = "openai", "response-1", false
	wrapper.InputTokens, wrapper.CostUSD, wrapper.Credits = 1000, Ptr(0.004), Ptr(7.0)
	in.RecordCall(span)
	for attempt := range 2 {
		if in.RecordCall(wrapper) {
			t.Fatalf("wrapper attempt %d counted as a new call", attempt)
		}
	}
	in.End(EndOptions{})
	auditAssertSubset(t, r.response(t), map[string]any{"llm_call_count": 1, "total_input_tokens": 1000, "total_cost_usd": 0.004, "total_credits": 7.0})
	if len(r.named("ai_llm_response_received")) != 1 {
		t.Fatal("replayed wrapper emitted another event")
	}
}

func TestCallMatchingSpecificHintSupersedesGenericAppError(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(fmt.Sprintf("finish=%t", finish), func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			span := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
			in.RecordCall(span)
			in.hintOutcome(StatusError, "app_error")
			wrapper := span
			wrapper.Integration, wrapper.ErrorType = "openai", "timeout"
			status, errorType := "timeout", "timeout"
			if finish {
				wrapper.ErrorType, wrapper.FinishReason = "", "length"
				status, errorType = "incomplete", "length"
			}
			in.RecordCall(wrapper)
			in.End(EndOptions{})
			auditAssertSubset(t, r.response(t), map[string]any{"response_status": status, "error_type": errorType})
		})
	}
}
