package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the example against the mock LLM server replaying the shared cassette.
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("examples/node-anthropic.json")
	if err != nil {
		return err
	}
	defer llm.Close()
	res, err := runSink(sink, option.WithBaseURL(llm.URL), option.WithAPIKey("check"), option.WithMaxRetries(0))
	if err != nil {
		return err
	}
	if n := llm.Pending(); n > 0 {
		return fmt.Errorf("%d cassette replies not served", n)
	}
	if want := "Yes, you can change it until the order ships."; res.Answer != want {
		return fmt.Errorf("answer %q, want %q", res.Answer, want)
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the prompt and the answer, field by field. Input tokens are the total:
// 22 uncached + 1024 cache read + 120 cache write.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 1, "ai_response_received": 1}); err != nil {
		return err
	}
	// 22 × $1 + 1024 × $0.10 + 120 × $1.25 + 14 × $5, per million tokens.
	const cost = 0.0003444
	checks := []struct {
		event string
		want  map[string]any
	}{
		{"ai_user_prompt_created", map[string]any{"feature": "support_bot", "surface": "web"}},
		{"ai_response_received", map[string]any{
			"response_status": "success", "trigger": "user", "primary_provider": "anthropic",
			"primary_model": "claude-haiku-4-5-20251001", "finish_reason": "end_turn", "llm_call_count": 1,
			"total_input_tokens": 1166, "total_output_tokens": 14, "total_tokens": 1180,
			"total_cached_input_tokens": 1024, "total_cache_write_tokens": 120, "total_cost_usd": cost,
			"time_to_first_token_ms": harness.Present,
			"usage_breakdown": []map[string]any{{
				"provider": "anthropic", "model": "claude-haiku-4-5-20251001",
				"call_count": 1, "input_tokens": 1166, "output_tokens": 14, "total_tokens": 1180, "cached_input_tokens": 1024,
				"cache_write_tokens": 120, "service_tier": "standard", "error_count": 0, "cost_usd": cost,
			}},
		}},
	}
	for _, c := range checks {
		m := byEvent[c.event][0]
		if got := harness.Integration(m); got != "anthropic" {
			return fmt.Errorf("%s: integration %q, want anthropic", c.event, got)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", c.event, m.UserID)
		}
		c.want["interaction_id"], c.want["conversation_id"] = res.InteractionID, "conv_address_1"
		if err := harness.Fields(c.event, m.Properties, c.want); err != nil {
			return err
		}
	}
	return nil
}
