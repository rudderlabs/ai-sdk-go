package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the job against the mock LLM server, which replays the shared cassette once per
// model call (the task's, then the sub-task's).
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("examples/node-anthropic.json", "examples/node-anthropic.json")
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
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the two task answers and the score, field by field. A task sends no prompt
// event; each answer counts only its own model call.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 0, "ai_response_received": 2, "ai_interaction_scored": 1}); err != nil {
		return err
	}
	answers := byEvent["ai_response_received"]
	usage := map[string]any{
		"response_status": "success", "trigger": "system", "primary_provider": "anthropic", "llm_call_count": 1,
		"total_input_tokens": 1166, "total_output_tokens": 14, "total_cached_input_tokens": 1024, "total_cache_write_tokens": 120,
		"total_cost_usd": 0.0003444, "account_id": "acct_acme", "on_behalf_of": "user_42", "feature": "ops_digest",
		"conversation_id": res.ConversationID,
	}
	checks := []struct {
		event, integration string
		props, want        map[string]any
	}{
		{"ai_response_received", "anthropic", harness.Find(answers, "interaction_id", res.TaskID), map[string]any{
			"task_name": "nightly-digest", "trigger_source": "schedule", "parent_interaction_id": harness.Absent,
		}},
		{"ai_response_received", "anthropic", harness.Find(answers, "interaction_id", res.ReviewID), map[string]any{
			"task_name": "digest-review", "trigger_source": "agent", "parent_interaction_id": res.TaskID,
		}},
		{"ai_interaction_scored", "manual", byEvent["ai_interaction_scored"][0].Properties, map[string]any{
			"interaction_id": res.TaskID, "conversation_id": res.ConversationID, "evaluation_name": "digest_quality",
			"score_value": 0.9, "evaluator": "code", "trigger": "system", "account_id": "acct_acme", "on_behalf_of": "user_42",
			"feature": "ops_digest",
		}},
	}
	for _, c := range checks {
		if c.event == "ai_response_received" {
			for k, v := range usage {
				c.want[k] = v
			}
		}
		if err := harness.Fields(c.event, c.props, c.want); err != nil {
			return err
		}
	}
	for _, m := range msgs {
		want := "anthropic"
		if m.Event == "ai_interaction_scored" {
			want = "manual"
		}
		if got := harness.Integration(m); got != want {
			return fmt.Errorf("%s: integration %q, want %q", m.Event, got, want)
		}
		if m.UserID != "" || m.AnonymousID != "ai_system" {
			return fmt.Errorf("%s: userId %q, anonymousId %q; want the anonymous system user", m.Event, m.UserID, m.AnonymousID)
		}
	}
	return nil
}
