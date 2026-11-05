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

func TestMergePricesWithWrapperDeployedModel(t *testing.T) {
	const deployedModel = "us.anthropic.claude-opus-5-20261101-v1:0"
	for _, counts := range [][2]int{{3, 1}, {1, 3}} {
		for _, spanFirst := range []bool{false, true} {
			for _, restore := range []bool{false, true} {
				t.Run(fmt.Sprintf("span=%d/wrapper=%d/spanFirst=%t/restore=%t", counts[0], counts[1], spanFirst, restore), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithPrices(map[string]ModelPrice{
						deployedModel:   {TokenPrice: TokenPrice{InputPerMtok: 2, OutputPerMtok: 4}},
						"claude-opus-5": {TokenPrice: TokenPrice{InputPerMtok: 20, OutputPerMtok: 40}},
					}), WithCredits(func(c CreditCall) (*float64, error) {
						if c.Model == deployedModel {
							if c.InputTokens != 1000 || c.OutputTokens != 500 || c.CostUSD == nil || *c.CostUSD != 0.004 {
								t.Errorf("wrapper pricing used different usage or cost: %+v", c)
							}
							return Ptr(7 * float64(c.CallCount)), nil
						}
						return Ptr(0.0), nil
					}))
					client := &pauseReentrantClient{recorder: r}
					_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
					span := LLMCall{Provider: "aws.bedrock", Model: "claude-opus-5", Integration: "tracing", StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), CallCount: counts[0], InputTokens: 10, OutputTokens: 5}
					wrapper := span
					wrapper.Model, wrapper.Integration, wrapper.CallCount = deployedModel, "anthropic", counts[1]
					wrapper.InputTokens, wrapper.OutputTokens = 1000, 500
					firstCall, secondCall := wrapper, span
					if spanFirst {
						firstCall, secondCall = span, wrapper
					}
					if restore {
						client.reenter = func() { in.RecordCall(firstCall) }
						snapshot, ok := in.Pause(PauseOptions{})
						if !ok || len(snapshot.PendingCalls) != 1 {
							t.Fatal("pause did not carry the first report", snapshot)
						}
						in = round3Resume(t, in, snapshot, true)
					} else {
						in.RecordCall(firstCall)
					}
					in.RecordCall(secondCall)
					if got := in.calls[0].rawModel; got != deployedModel {
						t.Errorf("merged pricing model = %q, want %q", got, deployedModel)
					}
					in.End(EndOptions{})
					auditAssertSubset(t, lastResponse(t, r), map[string]any{"primary_model": "claude-opus-5", "llm_call_count": 3, "total_input_tokens": 1000, "total_output_tokens": 500, "total_cost_usd": 0.004, "total_credits": 21.0})
					rows := lastResponse(t, r)["usage_breakdown"].([]map[string]any)
					if len(rows) != 1 {
						t.Fatalf("usage groups = %d, want 1", len(rows))
					}
					auditAssertSubset(t, rows[0], map[string]any{"model": "claude-opus-5", "call_count": 3, "cost_usd": 0.004, "credits": 21.0})
					if events := r.named("ai_llm_response_received"); len(events) != 1 {
						t.Fatalf("call events = %d, want 1", len(events))
					}
					auditAssertSubset(t, r.props(t, "ai_llm_response_received"), map[string]any{"model": "claude-opus-5", "cost_usd": 0.004})
				})
			}
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
