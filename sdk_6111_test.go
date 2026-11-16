package rudderstackai

// SDK-6111: overflow call events behind a provisional call, dated-model twins, and writes during paused delivery.

import (
	"context"
	"fmt"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func sdk6111Window(call LLMCall, startMs int64) LLMCall {
	call.StartedAt, call.EndedAt = time.UnixMilli(startMs), time.UnixMilli(startMs+10)
	return call
}

func sdk6111Num(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func TestSDK6111OverflowReportsBehindAProvisionalCallKeepTheFirstReportFlags(t *testing.T) {
	for _, field := range []string{"cost_usd", "credits", "server_tool_cost_usd"} {
		t.Run(field, func(t *testing.T) {
			model := "unpriced-model"
			opts := []ConfigOption{WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1})}
			if field == "server_tool_cost_usd" {
				model = "fee-model"
				opts = append(opts, WithPrices(map[string]ModelPrice{model: {TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1}, ServerToolPerCall: map[string]float64{"web_search": 0.01}}}))
			}
			r := setupAI(t, opts...)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			// A timed span holds call events; priced and credited, so the interaction totals depend on the overflow group alone.
			span := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "tracing", InputTokens: 10, CostUSD: Ptr(0.001), Credits: Ptr(1.0)}, 100)
			i.RecordCall(span)
			// Unpriced, uncredited or without fees, then priced, credited or with fees, in one breakdown group past the cap.
			first := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "openai", InputTokens: 20}, 200)
			second := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "openai", InputTokens: 30}, 300)
			switch field {
			case "cost_usd":
				second.CostUSD = Ptr(0.004)
			case "credits":
				second.Credits = Ptr(7.0)
			default:
				// A kind without a fee leaves the first call's fees unknown.
				first.ServerToolCalls = map[string]int{"code_execution": 1}
				second.ServerToolCalls = map[string]int{"web_search": 1}
			}
			i.RecordCall(first)
			i.RecordCall(second)
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("overflow call events were not held behind the span")
			}
			i.End(EndOptions{})
			found := 0
			for _, e := range r.named("ai_llm_response_received") {
				if e.Properties["input_tokens"] != 50 {
					continue
				}
				found++
				if v, ok := e.Properties[field]; ok {
					t.Errorf("overflow call event %s = %v, want absent", field, v)
				}
			}
			if found != 1 {
				t.Fatalf("overflow call events = %d, want 1", found)
			}
			resp := lastResponse(t, r)
			if field == "server_tool_cost_usd" {
				for _, group := range resp["usage_breakdown"].([]map[string]any) {
					if v, ok := group[field]; ok {
						t.Fatalf("usage_breakdown %s = %v, want absent", field, v)
					}
				}
				return
			}
			total := map[string]string{"cost_usd": "total_cost_usd", "credits": "total_credits"}[field]
			if v, ok := resp[total]; ok {
				t.Fatalf("response %s = %v, want absent", total, v)
			}
		})
	}
}

func TestSDK6111DatedModelSpanMatchesWrapper(t *testing.T) {
	wrapper := LLMCall{
		Provider: "openai", Model: "gpt-4o", Integration: IntegrationOpenAI, ResponseID: "resp_1",
		InputTokens: 10, OutputTokens: 2, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110),
	}
	// The same call's span, without a response ID and with the dated model ID.
	dated := wrapper
	dated.Integration, dated.ResponseID, dated.Model = "tracing", "", "gpt-4o-2024-08-06"
	for _, spanFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("spanFirst=%t", spanFirst), func(t *testing.T) {
			r := setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			reports := []LLMCall{wrapper, dated}
			if spanFirst {
				reports[0], reports[1] = reports[1], reports[0]
			}
			for _, c := range reports {
				i.RecordCall(c)
			}
			i.End(EndOptions{})
			resp := lastResponse(t, r)
			if sdk6111Num(resp["llm_call_count"]) != 1 || sdk6111Num(resp["total_input_tokens"]) != 10 {
				t.Fatalf("llm_call_count=%v total_input_tokens=%v, want 1 and 10", resp["llm_call_count"], resp["total_input_tokens"])
			}
		})
	}
}

// Claude IDs stay exact: a different Claude model is a different call.
func TestSDK6111SameModel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"gpt-4o", "gpt-4o-2024-08-06", true},
		{"GPT-4o-2024-08-06", "gpt-4o", true},
		{"gpt-4o", "", true},
		{"gpt-4o", "gpt-4.1", false},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4", "claude-sonnet-4-5", false},
	} {
		if got := sameModel(tc.a, tc.b); got != tc.want {
			t.Errorf("sameModel(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// As in Node and Python, a folded group reads as successful once any of its calls succeeded.
func TestSDK6111OverflowGroupWithASuccessfulCallSendsNoErrorType(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	// A timed span holds call events, so both overflow calls fold into one event.
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10}, 100))
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 20, ErrorType: "timeout"}, 200))
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 30}, 300))
	i.End(EndOptions{})
	found := 0
	for _, e := range r.named("ai_llm_response_received") {
		if e.Properties["input_tokens"] != 50 {
			continue
		}
		found++
		if v, ok := e.Properties["error_type"]; ok {
			t.Errorf("overflow call event error_type = %v, want absent", v)
		}
	}
	if found != 1 {
		t.Fatalf("overflow call events = %d, want 1", found)
	}
}

// sdk6111ReenterClient records once from inside delivery of the first matching event, as an app's
// synchronous transport callback can.
type sdk6111ReenterClient struct {
	*recorder
	event   string
	done    bool
	reenter func()
}

func (c *sdk6111ReenterClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok && !c.done && e.Event == c.event {
		c.done = true
		c.reenter()
	}
	return err
}

func TestSDK6111ToolRecordedDuringPausedCallDeliveryIsCountedOnceWithItsEvent(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "Looked Up"}}}))
	client := &sdk6111ReenterClient{recorder: r, event: "ai_llm_response_received"}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	// A timed span's call event waits until pause delivers it.
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10}, 100))
	client.reenter = func() { i.RecordToolCalls(ToolCall{ID: "call_1", Name: "lookup"}) }
	if _, ok := i.Pause(PauseOptions{}); !ok {
		t.Fatal("pause")
	}
	if !client.done {
		t.Fatal("call event was not delivered during pause")
	}
	i.Resume()
	i.End(EndOptions{})
	tools := 0.0
	for _, e := range r.named("ai_response_received") {
		tools += sdk6111Num(e.Properties["tool_call_count"])
	}
	if tools != 1 {
		t.Fatalf("tool_call_count across segments = %g, want 1", tools)
	}
	if n := len(r.named("Looked Up")); n != 1 {
		t.Fatalf("declared tool events = %d, want 1", n)
	}
}
