package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// runCheck runs the example against the mock LLM server replaying the recorded session.
func runCheck(sink *harness.Sink) error {
	path, err := harness.RepoFile("spec/fixtures/cassettes/claude-managed-agents-tools.json")
	if err != nil {
		return err
	}
	cassette, err := testkit.LoadCassette(path)
	if err != nil {
		return err
	}
	llm := testkit.StartLLM()
	defer llm.Close()
	llm.ReplayManagedAgents(cassette)
	res, err := runSink(sink, option.WithBaseURL(llm.URL), option.WithAPIKey("check"), option.WithMaxRetries(0))
	if err != nil {
		return err
	}
	if n := llm.Pending(); n > 0 {
		return fmt.Errorf("%d cassette replies not served", n)
	}
	if res.Answer != "Sunny" {
		return fmt.Errorf("answer %q, want Sunny", res.Answer)
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the prompt and the answer, field by field. Input tokens are the total of both
// requests: 6 uncached + 4283 cache read + 4590 cache write.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 1, "ai_response_received": 1}); err != nil {
		return err
	}
	// (4 × 0.1 + 4283 × 0.125 + 218 × 0.5) + (2 × 0.1 + 4283 × 0.01 + 307 × 0.125 + 6 × 0.5), per
	// million tokens, each call rounded to 8 decimals.
	const cost = 0.00072919
	checks := []struct {
		event string
		want  map[string]any
	}{
		{"ai_user_prompt_created", map[string]any{"feature": "managed_agents_example"}},
		{"ai_response_received", map[string]any{
			"response_status": "success", "trigger": "user", "primary_provider": "anthropic",
			"primary_model": "claude-haiku-5-5", "llm_call_count": 2, "tool_call_count": 2,
			"total_input_tokens": 8879, "total_output_tokens": 224, "total_tokens": 9103,
			"total_cached_input_tokens": 4283, "total_cache_write_tokens": 4590, "total_cost_usd": cost,
		}},
	}
	for _, c := range checks {
		m := byEvent[c.event][0]
		if got := harness.Integration(m); got != "claude-managed-agents" {
			return fmt.Errorf("%s: integration %q, want claude-managed-agents", c.event, got)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", c.event, m.UserID)
		}
		c.want["conversation_id"] = res.SessionID
		if err := harness.Fields(c.event, m.Properties, c.want); err != nil {
			return err
		}
	}
	return nil
}
