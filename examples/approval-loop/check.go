package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the agent against the mock LLM server, which replays the shared cassette once per
// model call (one per worker run).
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("examples/node-anthropic.json", "examples/node-anthropic.json", "examples/node-anthropic.json")
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

// expectEvents checks the one interaction: ai_task_started, two paused answers and the final one,
// each counting only its own model call.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 0, "ai_task_started": 1, "ai_response_received": 3}); err != nil {
		return err
	}
	ids := map[string]any{
		"interaction_id": res.InteractionID, "conversation_id": res.ConversationID, "task_name": "refund-approval",
		"trigger_source": "queue", "account_id": "acct_acme", "on_behalf_of": "user_42", "feature": "refunds",
	}
	if err := harness.Fields("ai_task_started", byEvent["ai_task_started"][0].Properties, ids); err != nil {
		return err
	}
	paused, final := 0, 0
	for _, m := range byEvent["ai_response_received"] {
		want := map[string]any{
			"trigger": "system", "primary_provider": "anthropic", "llm_call_count": 1,
			"total_input_tokens": 1166, "total_output_tokens": 14,
		}
		for k, v := range ids {
			want[k] = v
		}
		if m.Properties["response_status"] == "paused" {
			paused++
			want["pause_reason"], want["pause_count"], want["total_paused_ms"] = "approval", harness.Absent, harness.Absent
		} else {
			final++
			want["response_status"], want["pause_count"], want["total_paused_ms"] = "success", 2, harness.Present
		}
		if err := harness.Fields("ai_response_received", m.Properties, want); err != nil {
			return err
		}
	}
	if paused != 2 || final != 1 {
		return fmt.Errorf("got %d paused and %d final answers, want 2 and 1", paused, final)
	}
	for _, m := range msgs {
		want := "anthropic"
		if m.Event == "ai_task_started" {
			want = "manual" // sent before the first model call
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
