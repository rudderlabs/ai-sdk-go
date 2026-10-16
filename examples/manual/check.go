package main

import (
	"fmt"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the example: no provider is involved.
func runCheck(sink *harness.Sink) error {
	res, err := runSink(sink)
	if err != nil {
		return err
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks every event of the turn, field by field.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{
		"ai_user_prompt_created": 1, "ai_llm_response_received": 2, "ticket_created": 1,
		"ai_response_received": 1, "ai_interaction_classified": 1,
	}); err != nil {
		return err
	}
	calls := byEvent["ai_llm_response_received"]
	response := byEvent["ai_response_received"][0].Properties
	checks := []struct {
		event string
		props map[string]any
		want  map[string]any
	}{
		{"ai_user_prompt_created", byEvent["ai_user_prompt_created"][0].Properties, map[string]any{
			"feature": "helpdesk", "surface": "slack", "prompt_text": harness.Absent,
		}},
		{"ai_llm_response_received", harness.Find(calls, "response_id", "resp_tool"), map[string]any{
			"provider": "openai", "model": model, "input_tokens": 120, "output_tokens": 18, "total_tokens": 138,
			"finish_reason": "tool_calls", "cost_usd": 0.0000768, // 120 × $0.40 + 18 × $1.60, per million tokens
		}},
		{"ai_llm_response_received", harness.Find(calls, "response_id", "resp_answer"), map[string]any{
			"input_tokens": 160, "output_tokens": 25, "finish_reason": "stop", "cost_usd": 0.000104,
		}},
		{"ticket_created", byEvent["ticket_created"][0].Properties, map[string]any{
			"tool_name": "create_ticket", "tool_call_id": "call_1", "tool_status": "success", "tool_duration_ms": harness.Present,
			"priority": "high", "ticket_id": "T-1042", "trigger": "user",
		}},
		{"ai_response_received", response, map[string]any{
			"response_status": "success", "trigger": "user", "primary_provider": "openai", "primary_model": model,
			"finish_reason": "stop", "llm_call_count": 2, "tool_call_count": 1, "total_input_tokens": 280,
			"total_output_tokens": 43, "total_tokens": 323, "total_cost_usd": 0.0001808, "response_text": harness.Absent,
		}},
		{"ai_interaction_classified", byEvent["ai_interaction_classified"][0].Properties, map[string]any{
			"intent": "open_ticket", "outcome": "resolved", "intent_summary": "The user asked for a support ticket.",
			"classifier": "custom", "classifier_duration_ms": harness.Present, "trigger": "user",
		}},
	}
	for _, c := range checks {
		c.want["interaction_id"], c.want["conversation_id"] = res.InteractionID, "conv_helpdesk_1"
		if err := harness.Fields(c.event, c.props, c.want); err != nil {
			return err
		}
	}
	rows, _ := response["tool_breakdown"].([]any)
	if len(rows) != 1 {
		return fmt.Errorf("ai_response_received.tool_breakdown = %v, want one row", response["tool_breakdown"])
	}
	row, _ := rows[0].(map[string]any)
	if err := harness.Fields("tool_breakdown[0]", row, map[string]any{
		"tool_name": "create_ticket", "call_count": 1, "error_count": 0, "measured_count": 1, "duration_ms": harness.Present,
	}); err != nil {
		return err
	}
	for _, m := range msgs {
		if got := harness.Integration(m); got != "manual" {
			return fmt.Errorf("%s: integration %q, want manual", m.Event, got)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", m.Event, m.UserID)
		}
	}
	return nil
}
