package rudderstackai

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrimmingCustomPropertiesPreservesSDKFields(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sdkVersions bool
		padding     int
	}{
		{"sdk-small", true, 0},
		{"sdk-oversized", true, 40_000},
		{"custom-small", false, 0},
		{"custom-oversized", false, 40_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := map[string]any{
				"conversation_id": "c1", "interaction_id": "i1", "feature": "support", "surface": "web",
				"account_id": "acme", "on_behalf_of": "owner", "trigger_source": "api", "task_name": "lookup",
				"trigger_event_id": "e1", "parent_interaction_id": "parent", "trigger": "user", "agent_name": "planner",
				"tool_name": "search", "tool_call_id": "t1", "tool_status": "success", "tool_duration_ms": int64(12),
				"mcp_server": "crm", "result_size_bytes": 6,
			}
			r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{
				"search": {Event: "searched", Properties: func(map[string]any, any) map[string]any {
					custom := map[string]any{"agent_version": "custom-version", "variant": "custom-variant", "padding": strings.Repeat("x", tc.padding)}
					for k := range expected {
						custom[k] = "custom"
					}
					return custom
				}},
			}}))
			opts := InteractionOptions{
				UserID: "u1", ConversationID: "c1", InteractionID: "i1", Feature: "support", Surface: SurfaceWeb,
				AccountID: "acme", OnBehalfOf: "owner", TriggerSource: "api", TaskName: "lookup",
				TriggerEventID: "e1", ParentInteractionID: "parent",
			}
			if tc.sdkVersions {
				opts.AgentVersion, opts.Variant = "v1", "treatment"
			}
			run(t, opts, func(_ context.Context, i *Interaction) {
				i.RecordToolCalls(ToolCall{ID: "t1", Name: "search", AgentName: "planner", MCPServer: "crm"})
				i.RecordToolResult("t1", ToolResult{Error: Ptr(false), Duration: Ptr(12 * time.Millisecond), Result: "héllo"})
			})
			if tc.sdkVersions {
				expected["agent_version"], expected["variant"] = "v1", "treatment"
			}
			if tc.padding == 0 {
				expected["padding"] = ""
				if !tc.sdkVersions {
					expected["agent_version"], expected["variant"] = "custom-version", "custom-variant"
				}
			}
			if got := r.props(t, "searched"); !reflect.DeepEqual(got, expected) {
				t.Fatalf("properties: got %v, want %v", got, expected)
			}
		})
	}
}

func TestCreditsHookAndOverflow(t *testing.T) {
	r := setupAI(t, WithCredits(func(call CreditCall) (*float64, error) { return Ptr(2.0), nil }), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "plan"})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "answer", Credits: Ptr(3.0)})
	i.RecordCall(LLMCall{Model: "m", CallPurpose: "answer", Credits: Ptr(4.0)})
	i.End(EndOptions{})
	if p := r.props(t, "ai_response_received"); p["total_credits"] != 9.0 {
		t.Fatalf("credits: %v", p)
	}
}

func TestCreditsUnknownNeverEmitPartialTotal(t *testing.T) {
	r := setupAI(t, WithCredits(func(call CreditCall) (*float64, error) {
		if call.Model == "unpriced" {
			return nil, nil
		}
		return Ptr(2.0), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	i.RecordCall(LLMCall{Model: "unpriced"})
	i.End(EndOptions{})
	if _, has := r.props(t, "ai_response_received")["total_credits"]; has {
		t.Fatal("partial credits emitted")
	}
}

func TestCreditHookSkipsFailedCancelledAndUnknown(t *testing.T) {
	calls := 0
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { calls++; return Ptr(5.0), nil }))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	i.RecordCall(LLMCall{Model: "m", ErrorType: "rate_limit"})
	i.RecordCall(LLMCall{Model: "m", ErrorType: "cancelled", Credits: Ptr(9.0)})
	i.RecordCall(LLMCall{Model: "m", UsageUnknown: true})
	i.End(EndOptions{})
	if calls != 1 {
		t.Fatalf("hook called %d times", calls)
	}
	if _, has := r.props(t, "ai_response_received")["total_credits"]; has {
		t.Fatal("partial credits emitted")
	}
}

func TestInvalidCredits(t *testing.T) {
	for _, amount := range []float64{-1, math.Inf(1), math.NaN()} {
		if creditsOf(&amount) != nil {
			t.Fatal("invalid credits accepted")
		}
	}
}

func TestCallTimingPreservesZeroAndFalse(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	now := time.Now()
	i.RecordCall(LLMCall{Model: "m", StartedAt: now, EndedAt: now, FirstTokenAt: now, FallbackUsed: Ptr(false), BYOK: Ptr(false)})
	i.End(EndOptions{})
	p := r.props(t, "ai_llm_response_received")
	if p["latency_ms"] != int64(0) || p["time_to_first_token_ms"] != int64(0) || p["fallback_used"] != false || p["byok"] != false {
		t.Fatalf("timing and flags: %v", p)
	}
}

func TestCreditHookCanInspectAndUpdateInteraction(t *testing.T) {
	var i *Interaction
	blocked := false
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) {
		done := make(chan struct{})
		go func() {
			_ = i.Ended()
			i.SetConfidence(ConfidenceHigh)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			blocked = true
		}
		return Ptr(2.0), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Model: "m"})
	if blocked {
		t.Fatal("credit hook could not call locking interaction methods")
	}
	i.End(EndOptions{})
	if p := r.props(t, "ai_response_received"); p["confidence"] != "high" || p["total_credits"] != 2.0 {
		t.Fatalf("hook changes: %v", p)
	}
}

func TestCallLatencyExcludesCreditHook(t *testing.T) {
	var hookStart time.Time
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
		hookStart = time.Now()
		time.Sleep(60 * time.Millisecond)
		return Ptr(2.0), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	started := time.Now()
	i.RecordCall(LLMCall{Model: "m", StartedAt: started})
	i.End(EndOptions{})
	latency := r.props(t, "ai_llm_response_received")["latency_ms"].(int64)
	if latency > hookStart.Sub(started).Milliseconds()+1 {
		t.Fatalf("provider latency %d ms includes credit hook", latency)
	}
}

func TestCreditHookCanEndInteraction(t *testing.T) {
	var i *Interaction
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { i.End(EndOptions{}); return Ptr(2.0), nil }))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	if i.RecordCall(LLMCall{Model: "m"}) {
		t.Fatal("call recorded after hook ended interaction")
	}
	if p := r.props(t, "ai_response_received"); p["llm_call_count"] != 0 {
		t.Fatalf("calls: %v", p)
	}
}

func TestCreditHookReentrantResponseKeepsAgent(t *testing.T) {
	var i *Interaction
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) {
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "same", Credits: Ptr(0.0)})
		return Ptr(2.0), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	if i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "same", AgentName: "planner"}) {
		t.Fatal("duplicate recorded")
	}
	i.End(EndOptions{})
	p := r.props(t, "ai_response_received")
	if p["usage_breakdown"].([]map[string]any)[0]["agent_name"] != "planner" {
		t.Fatalf("agent: %v", p)
	}
	if p["llm_call_count"] != 1 || p["total_credits"] != 0.0 {
		t.Fatalf("calls: %v", p)
	}
}

// The probe avoids hanging the suite when a marshaler would reenter a locked interaction.
type interactionResultProbe struct {
	interaction *Interaction
	unlocked    bool
}

func (p *interactionResultProbe) MarshalJSON() ([]byte, error) {
	if p.interaction.mu.TryLock() {
		p.interaction.mu.Unlock()
		p.unlocked = true
		p.interaction.Ended()
	}
	return []byte(`{"ok":true}`), nil
}
func TestToolResultMarshalerCanReenter(t *testing.T) {
	r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_done"}}}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	probe := &interactionResultProbe{interaction: i}
	i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
	i.RecordToolResult("t", ToolResult{Result: probe})
	i.End(EndOptions{})
	if !probe.unlocked {
		t.Fatal("tool result marshaled under interaction lock")
	}
	if got := r.props(t, "lookup_done")["result_size_bytes"]; got != 11 {
		t.Fatal(got)
	}
}
