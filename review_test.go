package rudderstackai

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestReviewUnsetToolFields(t *testing.T) {
	for _, padding := range []int{0, 40000} {
		r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{
			"search": {Event: "searched", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"mcp_server": "custom", "result_size_bytes": 7, "padding": strings.Repeat("x", padding)}
			}},
		}}))
		run(t, InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) { i.RecordToolCalls(ToolCall{Name: "search"}) })
		p := r.props(t, "searched")
		if padding == 0 && (p["mcp_server"] != "custom" || p["result_size_bytes"] != 7) {
			t.Fatal(p)
		}
		if padding > 0 && (p["mcp_server"] != nil || p["result_size_bytes"] != nil) {
			t.Fatal(p)
		}
	}
}

func TestReviewInvalidCreditHookReports(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		seen := &seenErrors{}
		setupAI(t, WithOnError(seen.hook))
		if priceCredits(func(CreditCall) (*float64, error) { return &value, nil }, CreditCall{}) != nil {
			t.Fatal("accepted invalid credits")
		}
		if got := seen.all(); len(got) != 1 || got[0].Kind != KindUserCallback {
			t.Fatal(got)
		}
	}
}

func TestReviewDuplicateWrapperMetadata(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", InputTokens: 5})
	if i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", CallPurpose: "answer", BYOK: Ptr(true), FallbackUsed: Ptr(false), Credits: Ptr(2.0)}) {
		t.Fatal("duplicate counted")
	}
	if i.callCount != 1 || i.calls[0].callPurpose != "answer" || i.calls[0].byok == nil || i.calls[0].fallbackUsed == nil || i.calls[0].credits == nil || *i.calls[0].credits != 2 {
		t.Fatal(i.calls[0])
	}
	i.End(EndOptions{})
	if r.props(t, "ai_response_received")["total_credits"] != 2.0 {
		t.Fatal(r.props(t, "ai_response_received"))
	}
}

func TestReviewDuplicateInvalidCreditsClear(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", Credits: Ptr(2.0)})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", Credits: Ptr(-1.0)})
	if i.calls[0].credits != nil {
		t.Fatal("stale credits retained")
	}
	i.End(EndOptions{})
}

func TestReviewSampleOwned(t *testing.T) {
	for _, rates := range []map[string]float64{nil, {"event": 1}} {
		p := map[string]any{"sample_rate": 0.1, "other": 1}
		out, kept := sample("event", p, rates)
		if !kept || out["sample_rate"] != nil || p["sample_rate"] != 0.1 {
			t.Fatal(out, p)
		}
	}
}
func TestReviewSamplingToolBudget(t *testing.T) {
	for _, mcp := range []bool{false, true} {
		for _, supplied := range []bool{false, true} {
			r := setupAI(t, WithSampleRates(map[string]float64{"searched": 0.5}))
			id := "3f2c9a4e-8b1d-4c6f-9e2a-7d5b1c0e4a93"
			base := map[string]any{"interaction_id": id, "tool_name": "search", "tool_status": "success"}
			if !mcp {
				base["conversation_id"] = "c"
				base["trigger"] = "user"
			}
			custom := map[string]any{"padding": ""}
			if supplied {
				custom["sample_rate"] = 0.1
			}
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			for k, v := range custom {
				p[k] = v
			}
			custom["padding"] = strings.Repeat("x", maxPropertiesBytes-propertiesBytes(p))
			rule := ToolEventRule{Event: "searched", Properties: func(map[string]any, any) map[string]any { return custom }}
			if mcp {
				rc := &reqCtx{opts: &options{Options: Options{Analytics: r}, rules: map[string]ToolEventRule{"search": rule}}, base: base, toolName: "search", tool: &toolState{}}
				rc.declaredEvent(nil, "success")
			} else {
				if err := Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": rule}})); err != nil {
					t.Fatal(err)
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", ConversationID: "c", InteractionID: id, NoPromptEvent: true})
				i.RecordToolCalls(ToolCall{Name: "search", Error: Ptr(false)})
				i.End(EndOptions{})
			}
			got := r.props(t, "searched")
			if propertiesBytes(got) > maxPropertiesBytes || got["sample_rate"] != 0.5 {
				t.Errorf("mcp=%v supplied=%v bytes=%d stamp=%v", mcp, supplied, propertiesBytes(got), got["sample_rate"])
			}
		}
	}
}
