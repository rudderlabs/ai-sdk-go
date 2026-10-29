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
