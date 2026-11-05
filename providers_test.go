package rudderstackai

import (
	"context"
	"testing"
)

// Cases beyond spec/fixtures/providers-v0.1.json: a Bedrock ARN, and a Vertex ID as the served model.
func TestRoutedModelCloudAnthropicIDs(t *testing.T) {
	cases := []struct{ requested, served, want string }{
		{"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5-20251001", ""},
		{"claude-haiku-4-5", "claude-haiku-4-5@20251001", ""},
		{"claude-haiku-4-5", "claude-sonnet-4-5@20250929", "claude-haiku-4-5"},
	}
	for _, c := range cases {
		if got := routedModel(c.requested, c.served); got != c.want {
			t.Errorf("routedModel(%q, %q) = %q, want %q", c.requested, c.served, got, c.want)
		}
	}
}

func TestAuditCanonicalModelIDs(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"us.anthropic.claude-opus-5", "claude-opus-5"},
		{"Claude-Opus-5", "claude-opus-5"},
		{"claude-sonnet-4-5@20250929", "claude-sonnet-4-5"},
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"claude-sonnet-5-preview", "claude-sonnet-5-preview"},
		{"claude-sonnet-5-1234", "claude-sonnet-5-1234"},
		{"claude-sonnet-5-v2", "claude-sonnet-5-v2"},
		{"us-gov.anthropic.claude-opus-5-v2:3", "claude-opus-5"},
		{"jp.anthropic.claude-opus-5:0", "claude-opus-5"},
		{"au.anthropic.claude-opus-5@v2", "claude-opus-5"},
		{"in.anthropic.claude-opus-5@002", "claude-opus-5"},
		{"US.ANTHROPIC.EU.ANTHROPIC.Claude-Opus-5-20251001-v1:0@20251101", "claude-opus-5"},
		{"global.jp.anthropic.anthropic.claude-opus-5-20251001-20251101:2:0", "claude-opus-5"},
		{"anthropic/claude-opus-5-20251001-v1:0", "claude-opus-5"},
		{"arn:aws:bedrock:us-east-1:123:inference-profile/us.anthropic.claude-opus-5", "claude-opus-5"},
		{"anthropic.claude-3-5-sonnet-latest-v1:0", "claude-3-5-sonnet-latest"},
		{"anthropic.claude-3-5-sonnet-latest-20251001", "claude-3-5-sonnet-latest"},
		{"claude-sonnet-5-vx:abc", "claude-sonnet-5-vx:abc"},
		{"claude-sonnet-5-2025100", "claude-sonnet-5-2025100"},
		{"CUSTOM-CLAUDE-MODEL", "custom-claude-model"},
		{"eu.anthropic.claude-sonnet-5-5-20251001-v2:1", "claude-sonnet-5-5"},
		{"apac.anthropic.claude-sonnet-5-20251001-v1:0", "claude-sonnet-5"},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"claude-haiku-4-5@20251001", "claude-haiku-4-5"},
		{"claude-3-5-sonnet-v2@20241022", "claude-3-5-sonnet-v2"},
		{"claude-3-5-sonnet-latest", "claude-3-5-sonnet-latest"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-haiku-4-5-2025-10-01", "claude-haiku-4-5-2025-10-01"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"gpt-4.1-mini-2025-04-14", "gpt-4.1-mini-2025-04-14"},
		{"openai/gpt-4o", "openai/gpt-4o"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"amazon.nova-pro-v1:0", "amazon.nova-pro-v1:0"},
		{"US.ANTHROPIC.GPT-4.1-mini-20251001-v1:0@20251101", "US.ANTHROPIC.GPT-4.1-mini-20251001-v1:0@20251101"},
		{"OpenAI/GPT-4O", "OpenAI/GPT-4O"},
		{"Gemini-2.5-Pro@002", "Gemini-2.5-Pro@002"},
		{"", ""},
	} {
		if got := canonicalModel(tc.raw); got != tc.want {
			t.Errorf("canonicalModel(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestAuditCanonicalGroupingAndDedupOrders(t *testing.T) {
	const raw = "us.anthropic.claude-opus-5-20251001-v1:0"
	const canonical = "claude-opus-5"
	for _, reverse := range []bool{false, true} {
		for _, dedup := range []bool{false, true} {
			for _, limit := range []int{1, 100} {
				r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: limit}))
				_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				models := []string{raw, canonical, "claude-opus-5@20251001"}
				if reverse {
					models[0], models[1] = models[1], models[0]
				}
				count := len(models)
				for _, model := range models {
					id := ""
					if dedup {
						id, count = "shared-response", 1
					}
					in.RecordCall(LLMCall{Provider: "aws.bedrock", Model: model, ResponseID: id, InputTokens: 100, OutputTokens: 10, CostUSD: Ptr(0.25)})
				}
				in.End(EndOptions{})
				p := r.response(t)
				usage := p["usage_breakdown"].([]map[string]any)
				if p["primary_model"] != canonical || p["llm_call_count"] != count || p["total_input_tokens"] != count*100 || p["total_cost_usd"] != float64(count)*0.25 ||
					len(usage) != 1 || usage[0]["model"] != canonical || usage[0]["call_count"] != count || len(r.named("ai_llm_response_received")) != count {
					t.Fatalf("reverse=%t dedup=%t limit=%d: %v", reverse, dedup, limit, p)
				}
				for _, event := range r.named("ai_llm_response_received") {
					if event.Properties["model"] != canonical {
						t.Fatal(event.Properties)
					}
				}
				if limit == 1 && !dedup && (len(in.overflow) != 1 || in.overflow[0].model != canonical || in.overflow[0].callCount != 2) {
					t.Fatal("overflow did not use canonical group")
				}
			}
		}
	}
}

func TestAuditCanonicalFamiliesStayDistinct(t *testing.T) {
	r := setupAI(t)
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for _, model := range []string{"claude-sonnet-5", "claude-sonnet-5-5", "claude-3-5-sonnet", "claude-3-5-sonnet-latest"} {
		in.RecordCall(LLMCall{Provider: "anthropic", Model: model})
	}
	in.End(EndOptions{})
	if usage := r.response(t)["usage_breakdown"].([]map[string]any); len(usage) != 4 {
		t.Fatal(usage)
	}
}

func TestAuditCanonicalUsageAndPricing(t *testing.T) {
	var pricedModels []string
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(call CreditCall) (*float64, error) {
		pricedModels = append(pricedModels, call.Model)
		return Ptr(1.0), nil
	}))
	ClearPrices()
	if err := RegisterPrice("claude-haiku-4-5", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 5}}); err != nil {
		t.Fatal(err)
	}
	// An exact deployment price remains more specific than the family price.
	const deployed = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	if err := RegisterPrice(deployed, ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 2, OutputPerMtok: 10}}); err != nil {
		t.Fatal(err)
	}
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for _, model := range []string{deployed, "claude-haiku-4-5", "claude-haiku-4-5@20251001"} {
		in.RecordCall(LLMCall{Provider: "aws.bedrock", Model: model, InputTokens: 100, OutputTokens: 10})
	}
	in.End(EndOptions{})
	p := r.response(t)
	if p["primary_model"] != "claude-haiku-4-5" || p["total_cost_usd"] != 0.0006 {
		t.Fatal(p)
	}
	usage := p["usage_breakdown"].([]map[string]any)
	if len(usage) != 1 || usage[0]["model"] != "claude-haiku-4-5" || usage[0]["call_count"] != 3 {
		t.Fatal(usage)
	}
	for _, call := range r.named("ai_llm_response_received") {
		if call.Properties["model"] != "claude-haiku-4-5" {
			t.Fatal(call.Properties)
		}
	}
	if len(pricedModels) != 3 || pricedModels[0] != deployed || pricedModels[1] != "claude-haiku-4-5" || pricedModels[2] != "claude-haiku-4-5@20251001" {
		t.Fatalf("credit hook received canonical rather than raw IDs: %v", pricedModels)
	}
}

func TestAuditCanonicalRestoredModel(t *testing.T) {
	r := setupAI(t)
	snapshot := PausedInteraction{InteractionID: "restored", ConversationID: "c", Trigger: TriggerUser, UserID: "u", PauseCount: 1,
		PendingCalls: []map[string]any{{"provider": "aws.bedrock", "model": "us.anthropic.claude-opus-5", "call_count": 1, "last": true, "last_ok": true}}}
	_, in := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
	in.End(EndOptions{})
	p := r.response(t)
	if p["primary_model"] != "claude-opus-5" || p["usage_breakdown"].([]map[string]any)[0]["model"] != "claude-opus-5" {
		t.Fatal(p)
	}
}
