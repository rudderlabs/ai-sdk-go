package rudderstackai

import (
	"context"
	"math"
	"strings"
	"testing"
)

func mustPrice(t *testing.T, model string, p ModelPrice) {
	t.Helper()
	if err := RegisterPrice(model, p); err != nil {
		t.Fatal(err)
	}
}

func rate(in, out float64) ModelPrice {
	return ModelPrice{TokenPrice: TokenPrice{InputPerMtok: in, OutputPerMtok: out}}
}

// cost is EstimateCost with -1 for unpriced.
func cost(provider, model string, in, out int) float64 {
	c, ok := EstimateCost(provider, model, in, out, 0, 0)
	if !ok {
		return -1
	}
	return c
}

func TestPriceMatchesVersionSuffixes(t *testing.T) {
	setupAI(t)
	mustPrice(t, "gpt-4o", rate(2.5, 10))
	mustPrice(t, "gpt-4o-mini", rate(0.15, 0.6))
	for model, want := range map[string]float64{
		"gpt-4o":                 2.5,
		"GPT-4o-2024-08-06":      2.5,
		"gpt-4o-20240806":        2.5,
		"gpt-4o-0613":            2.5,
		"gpt-4o-v2.1":            2.5,
		"gpt-4o-latest":          2.5,
		"gpt-4o:free":            2.5,
		"gpt-4o@2024":            2.5,
		"openai/gpt-4o":          2.5,
		"gpt-4o-mini-2024-07-18": 0.15,
		"gpt-4o-audio":           -1,
		"gpt-4":                  -1,
		"openai/":                -1,
		"":                       -1,
	} {
		if got := cost("openai", model, 1e6, 0); got != want {
			t.Errorf("%q: got %v, want %v", model, got, want)
		}
	}
}

func TestPriceVendorPrefixAndProviderRank(t *testing.T) {
	setupAI(t)
	mustPrice(t, "gpt-4o", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 99, OutputPerMtok: 99}, Provider: "openai"})
	mustPrice(t, "openai/gpt-4o", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 3, OutputPerMtok: 7}, Provider: "openai"})
	if got := cost("openai", "openai/gpt-4o", 1e6, 1e6); got != 10 {
		t.Fatal(got)
	}
	if got := cost("openai", "openai/gpt-4o-mini", 1e6, 0); got != -1 {
		t.Fatal(got)
	}

	ClearPrices()
	mustPrice(t, "claude-haiku-4-5", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 2, OutputPerMtok: 10}, Provider: "anthropic"})
	mustPrice(t, "claude-haiku-4-5", rate(1, 5))
	mustPrice(t, "in-house", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1}, Provider: "acme"})
	if got := cost("anthropic", "claude-haiku-4-5", 1e6, 0); got != 2 {
		t.Fatalf("provider price: %v", got)
	}
	if got := cost("aws.bedrock", "claude-haiku-4-5", 1e6, 0); got != 1 {
		t.Fatalf("generic price: %v", got)
	}
	if got := cost("other", "in-house-v2", 1e6, 0); got != -1 {
		t.Fatalf("another provider's price: %v", got)
	}
	if got := cost("ACME", "in-house", 1e6, 1e6); got != 2 {
		t.Fatal(got)
	}
}

func TestPriceGeminiAPIAndVertexAreSeparate(t *testing.T) {
	setupAI(t)
	mustPrice(t, "gemini-2.5-flash", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, Provider: "google"})
	for provider, want := range map[string]float64{"google": 1, "gcp.gemini": 1, "google.vertex": -1, "gcp.vertex_ai": -1, "openai": -1} {
		if got := cost(provider, "gemini-2.5-flash-001", 1e6, 0); got != want {
			t.Errorf("%s: got %v, want %v", provider, got, want)
		}
	}
	mustPrice(t, "gemini-2.5-flash", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 3}, Provider: "google.vertex"})
	mustPrice(t, "gemini-2.5-flash", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 5}, Provider: "gcp.vertex_ai"})
	for provider, want := range map[string]float64{"google": 1, "gcp.gemini": 1, "google.vertex": 3, "gcp.vertex_ai": 5} {
		if got := cost(provider, "gemini-2.5-flash", 1e6, 0); got != want {
			t.Errorf("%s: got %v, want %v", provider, got, want)
		}
	}
}

func TestPriceBedrockAndAzureModelIDs(t *testing.T) {
	setupAI(t, WithAzureDeploymentModels(map[string]string{"Support-Prod": "GPT-4o"}))
	mustPrice(t, "nova-pro", rate(0.8, 3.2))
	mustPrice(t, "claude-sonnet-4-5", rate(3, 15))
	mustPrice(t, "anthropic.claude-sonnet-4-5", rate(4, 15))
	mustPrice(t, "us.anthropic.claude-sonnet-4-5", rate(5, 15))
	mustPrice(t, "gpt-4o", rate(2.5, 10))
	for _, c := range []struct {
		provider, model string
		want            float64
	}{
		{"aws.bedrock", "amazon.nova-pro-v1:0", 0.8},
		{"aws.bedrock", "us.amazon.nova-pro-v1:0", 0.8},
		{"aws.bedrock", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", 5},
		{"aws.bedrock", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0", 4},
		{"aws.bedrock", "global.anthropic.claude-sonnet-4-5-20250929-v1:0", 4},
		{"aws.bedrock", "claude-sonnet-4-5-20250929-v1:0", 3},
		{"aws.bedrock", "xx.anthropic.us.claude-sonnet-4-5", -1},
		{"anthropic", "eu.anthropic.claude-sonnet-4-5", -1},
		{"gcp.vertex_ai", "claude-sonnet-4-5@20250929", 3},
		{"azure.ai.openai", "support-prod", 2.5},
		{"azure.ai.openai", "SUPPORT-PROD", 2.5},
		{"azure.ai.openai", "unmapped", -1},
		{"openai", "support-prod", -1},
	} {
		if got := cost(c.provider, c.model, 1e6, 0); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.provider, c.model, got, c.want)
		}
	}
	if err := Configure(WithAzureDeploymentModels(nil)); err != nil {
		t.Fatal(err)
	}
	if got := cost("azure.ai.openai", "support-prod", 1e6, 0); got != -1 {
		t.Fatal(got)
	}
}

func TestPriceCacheLongContextTiersAndServerTools(t *testing.T) {
	setupAI(t)
	mustPrice(t, "claude", ModelPrice{
		TokenPrice:      TokenPrice{InputPerMtok: 3, OutputPerMtok: 15, CachedInputPerMtok: Ptr(0.3), CacheWritePerMtok: Ptr(3.75), CacheWrite1hPerMtok: Ptr(6.0)},
		TierMultipliers: map[string]float64{"batch": 0.5, "fast": 2},
		LongContext: &LongContextPrice{
			TokenPrice:      TokenPrice{InputPerMtok: 6, OutputPerMtok: 22.5},
			ThresholdTokens: 200_000,
		},
		ServerToolPerCall: map[string]float64{"web_search": 0.01},
	})
	mustPrice(t, "plain", rate(3, 15))

	// 1M input = 100k uncached + 400k cache read + 500k cache write (100k of them 1h).
	got, fees, ok := priceCall("anthropic", "claude", 100_000, 0, 0, 0, 0, "", nil)
	if !ok || got != 0.3 || fees != nil {
		t.Fatalf("base: %v %v %v", got, fees, ok)
	}
	got, _, _ = priceCall("anthropic", "plain", 1_000_000, 0, 0, 500_000, 0, "", nil)
	if got != 3 {
		t.Fatalf("cache writes fall back to input: %v", got)
	}
	got, _, _ = priceCall("anthropic", "claude", 200_000, 0, 50_000, 100_000, 20_000, "", nil)
	// uncached 50k*3 + cached 50k*0.3 + 5m writes 80k*3.75 + 1h writes 20k*6, per million
	if want := round8((50_000*3 + 50_000*0.3 + 80_000*3.75 + 20_000*6.0) / 1e6); got != want {
		t.Fatalf("cache: %v want %v", got, want)
	}
	// Above the threshold every token uses the long-context rates; cache rates fall back to its input.
	got, _, _ = priceCall("anthropic", "claude", 300_000, 1000, 100_000, 0, 0, "", nil)
	if want := round8((300_000*6 + 1000*22.5) / 1e6); got != want {
		t.Fatalf("long context: %v want %v", got, want)
	}
	got, _, _ = priceCall("anthropic", "claude", 1_000_000, 0, 0, 0, 0, "batch", nil)
	if got != 3 { // long context 6 * batch 0.5
		t.Fatalf("batch: %v", got)
	}
	got, _, _ = priceCall("anthropic", "claude", 100_000, 0, 0, 0, 0, "flex", nil)
	if got != 0.3 {
		t.Fatalf("a tier without a multiplier is priced at 1: %v", got)
	}
	got, fees, _ = priceCall("anthropic", "claude", 100_000, 0, 0, 0, 0, "fast", map[string]int{"web_search": 3, "unused": 0})
	if got != 0.63 || fees == nil || *fees != 0.03 {
		t.Fatalf("fees: %v %v", got, fees)
	}
	got, fees, _ = priceCall("anthropic", "claude", 100_000, 0, 0, 0, 0, "", map[string]int{"web_search": 1, "code_execution": 1})
	if got != 0.3 || fees != nil {
		t.Fatalf("an unpriced kind drops every fee: %v %v", got, fees)
	}
	got, fees, _ = priceCall("anthropic", "plain", 100_000, 0, 0, 0, 0, "", map[string]int{"web_search": 1})
	if got != 0.3 || fees != nil {
		t.Fatalf("no fees: %v %v", got, fees)
	}
	if _, _, ok := priceCall("anthropic", "unknown", 1, 1, 0, 0, 0, "", nil); ok {
		t.Fatal("unknown model priced")
	}
}

func TestRegisterPriceValidation(t *testing.T) {
	setupAI(t)
	bad := []float64{-1, math.NaN(), math.Inf(1)}
	var cases []ModelPrice
	for _, v := range bad {
		cases = append(cases,
			rate(v, 1), rate(1, v),
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1, CachedInputPerMtok: Ptr(v)}},
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1, CacheWritePerMtok: Ptr(v)}},
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1, CacheWrite1hPerMtok: Ptr(v)}},
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, TierMultipliers: map[string]float64{"batch": v}},
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, LongContext: &LongContextPrice{TokenPrice: TokenPrice{InputPerMtok: v}}},
			ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, ServerToolPerCall: map[string]float64{"web_search": v}},
		)
	}
	cases = append(cases,
		ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, TierMultipliers: map[string]float64{"priority": 2}},
		ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1}, LongContext: &LongContextPrice{ThresholdTokens: -1}},
	)
	for n, p := range cases {
		if err := RegisterPrice("m", p); err == nil || !strings.HasPrefix(err.Error(), "rudderstackai: RegisterPrice:") {
			t.Errorf("case %d: want an error, got %v", n, err)
		}
		if err := Configure(WithCaptureContent(true), WithPrices(map[string]ModelPrice{"m": p})); err == nil {
			t.Errorf("case %d: Configure accepted a bad price", n)
		}
	}
	if err := RegisterPrice("", rate(1, 1)); err == nil {
		t.Fatal("empty model accepted")
	}
	if _, ok := lookupPrice("", "m"); ok {
		t.Fatal("a bad price was registered")
	}
	if cfg().captureContent {
		t.Fatal("a failed Configure changed a setting")
	}
	if err := Configure(WithPrices(map[string]ModelPrice{"gpt-4.1": rate(2, 8)}), WithPrices(map[string]ModelPrice{"gpt-4.1-mini": rate(0.4, 1.6)})); err != nil {
		t.Fatal(err)
	}
	if cost("openai", "gpt-4.1-mini-2025-04-14", 1e6, 0) != 0.4 || cost("openai", "gpt-4.1-2025-04-14", 1e6, 0) != 2 {
		t.Fatal("Configure prices not merged")
	}
	ClearPrices()
	if cost("openai", "gpt-4.1", 1e6, 0) != -1 {
		t.Fatal("ClearPrices kept a price")
	}
}

func TestReportedCost(t *testing.T) {
	for _, c := range []struct {
		in   any
		want float64
		ok   bool
	}{
		{0.5, 0.5, true},
		{0.0, 0, true},
		{2, 2, true},
		{"0.0012", 0.0012, true},
		{" 1.2e-05 ", 1.2e-05, true},
		{"", 0, false},
		{"  ", 0, false},
		{"abc", 0, false},
		{-1.0, 0, false},
		{"-1", 0, false},
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
		{"Inf", 0, false},
		{nil, 0, false},
		{true, 0, false},
	} {
		got, ok := ReportedCost(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ReportedCost(%#v) = %v, %v", c.in, got, ok)
		}
	}
}

func TestRound8HalfUp(t *testing.T) {
	for in, want := range map[float64]float64{
		0.123456785:    0.12345679,
		0.000000125:    0.00000013,
		0.000000124999: 0.00000012,
		1.5:            1.5,
		0:              0,
	} {
		if got := round8(in); got != want {
			t.Errorf("round8(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCallCostsOnEvents(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	mustPrice(t, "m", ModelPrice{TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 2}, TierMultipliers: map[string]float64{"batch": 0.5}, ServerToolPerCall: map[string]float64{"web_search": 0.01}})
	run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) {
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1e6, OutputTokens: 1e6, ServiceTier: "batch", ServerToolCalls: map[string]int{"web_search": 2}})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1e6, CostUSD: Ptr(0.0)})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1e6, CostUSD: Ptr(-1.0)})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1e6, UsageUnknown: true, ErrorType: "rate_limit"})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1e6, CostUSD: Ptr(5.0), ErrorType: "cancelled"})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1})
	})
	calls := r.named("ai_llm_response_received")
	if len(calls) != 6 {
		t.Fatal(len(calls))
	}
	first := calls[0].Properties
	if first["cost_usd"] != 1.52 || first["server_tool_cost_usd"] != 0.02 || first["server_tool_call_count"] != 2 || first["service_tier"] != "batch" {
		t.Fatalf("priced call: %v", first)
	}
	if calls[1].Properties["cost_usd"] != 0.0 || calls[2].Properties["cost_usd"] != 1.0 {
		t.Fatalf("reported 0 kept, negative ignored: %v %v", calls[1].Properties, calls[2].Properties)
	}
	for _, n := range []int{3, 4} {
		if _, has := calls[n].Properties["cost_usd"]; has {
			t.Fatalf("failed or cancelled call priced: %v", calls[n].Properties)
		}
	}
	resp := r.response(t)
	if resp["total_cost_usd"] != round8(1.52+0+1+0.000001) {
		t.Fatalf("total: %v", resp["total_cost_usd"])
	}
	b := resp["usage_breakdown"].([]map[string]any)
	if len(b) != 2 || b[0]["server_tool_cost_usd"] != 0.02 || b[1]["service_tier"] != nil {
		t.Fatalf("breakdown: %v", b)
	}
}
