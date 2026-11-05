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
		{"eu.anthropic.claude-sonnet-5-5-20251001-v2:1", "claude-sonnet-5-5"},
		{"apac.anthropic.claude-sonnet-5-20251001-v1:0", "claude-sonnet-5"},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"claude-haiku-4-5@20251001", "claude-haiku-4-5"},
		{"claude-3-5-sonnet-v2@20241022", "claude-3-5-sonnet"},
		{"claude-3-5-sonnet-latest", "claude-3-5-sonnet"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-haiku-4-5-2025-10-01", "claude-haiku-4-5"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"gpt-4.1-mini-2025-04-14", "gpt-4.1-mini-2025-04-14"},
		{"openai/gpt-4o", "openai/gpt-4o"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"amazon.nova-pro-v1:0", "amazon.nova-pro-v1:0"},
	} {
		if got := canonicalModel(tc.raw); got != tc.want {
			t.Errorf("canonicalModel(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestAuditCanonicalUsageAndPricing(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
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
