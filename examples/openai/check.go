package main

import (
	"fmt"

	"github.com/openai/openai-go/v3/option"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the example against the mock LLM server replaying the shared cassette.
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("examples/node-openai.json")
	if err != nil {
		return err
	}
	defer llm.Close()
	res, err := runSink(sink, option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("check"), option.WithMaxRetries(0),
		option.WithUnsafeAllowHTTP()) // the mock listens on loopback HTTP
	if err != nil {
		return err
	}
	if n := llm.Pending(); n > 0 {
		return fmt.Errorf("%d cassette replies not served", n)
	}
	if want := "Standard shipping usually takes 3 to 5 business days."; res.Answer != want {
		return fmt.Errorf("answer %q, want %q", res.Answer, want)
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the prompt, the answer and the feedback, field by field.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 1, "ai_response_received": 1, "ai_user_action": 1}); err != nil {
		return err
	}
	ids := map[string]any{"interaction_id": res.InteractionID, "conversation_id": "conv_shipping_1"}
	checks := []struct {
		event, integration string
		want               map[string]any
	}{
		{"ai_user_prompt_created", "openai", map[string]any{"feature": "support_bot", "surface": "web", "input_method": "text", "prompt_text": harness.Absent}},
		{"ai_response_received", "openai", map[string]any{
			"response_status": "success", "trigger": "user", "feature": "support_bot", "surface": "web",
			"primary_provider": "openai", "primary_model": "gpt-4.1-mini-2025-04-14", "finish_reason": "stop",
			"llm_call_count": 1, "tool_call_count": 0, "total_input_tokens": 29, "total_output_tokens": 11, "total_tokens": 40,
			"total_cached_input_tokens": 0, "total_cost_usd": 0.0000292, "latency_ms": harness.Present,
			"time_to_first_token_ms": harness.Present, "usage_breakdown": harness.Present, "response_text": harness.Absent,
		}},
		{"ai_user_action", "manual", map[string]any{
			"action_type": "feedback_given", "surface": "web", "action_details": map[string]any{"feedback_type": "thumbs", "feedback_value": 1},
		}},
	}
	for _, c := range checks {
		m := byEvent[c.event][0]
		if got := harness.Integration(m); got != c.integration {
			return fmt.Errorf("%s: integration %q, want %q", c.event, got, c.integration)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", c.event, m.UserID)
		}
		for k, v := range ids {
			c.want[k] = v
		}
		if err := harness.Fields(c.event, m.Properties, c.want); err != nil {
			return err
		}
	}
	return nil
}
