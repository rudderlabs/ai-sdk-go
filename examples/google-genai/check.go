package main

import (
	"fmt"

	"google.golang.org/genai"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runCheck runs the example against the mock LLM server replaying the shared cassette.
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("google-genai.json")
	if err != nil {
		return err
	}
	defer llm.Close()
	res, err := runSink(sink, &genai.ClientConfig{APIKey: "check", HTTPOptions: genai.HTTPOptions{BaseURL: llm.URL}})
	if err != nil {
		return err
	}
	if n := llm.Pending(); n > 0 {
		return fmt.Errorf("%d cassette replies not served", n)
	}
	if want := "The weather in Paris is sunny with a temperature of 20°C."; res.Answer != want {
		return fmt.Errorf("answer %q, want %q", res.Answer, want)
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the prompt and the answer, field by field: two calls (the function call,
// then the streamed answer, whose usage is the last running total) and one tool call.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 1, "ai_response_received": 1}); err != nil {
		return err
	}
	checks := []struct {
		event string
		want  map[string]any
	}{
		{"ai_user_prompt_created", map[string]any{"feature": "travel_assistant", "surface": "mobile"}},
		{"ai_response_received", map[string]any{
			"response_status": "success", "trigger": "user", "primary_provider": "google", "primary_model": model,
			"finish_reason": "stop", "llm_call_count": 2, "tool_call_count": 1,
			"total_input_tokens": 55 + 99, "total_output_tokens": 16 + 16, "total_tokens": 186, "total_cached_input_tokens": 0,
			// (154 × $0.10 + 32 × $0.40) per million tokens.
			"total_cost_usd": 0.0000282, "time_to_first_token_ms": harness.Present,
			"tool_breakdown": []map[string]any{{"tool_name": "get_weather", "call_count": 1, "error_count": 0}},
		}},
	}
	for _, c := range checks {
		m := byEvent[c.event][0]
		if got := harness.Integration(m); got != "google-genai" {
			return fmt.Errorf("%s: integration %q, want google-genai", c.event, got)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", c.event, m.UserID)
		}
		c.want["interaction_id"], c.want["conversation_id"] = res.InteractionID, "conv_weather_1"
		if err := harness.Fields(c.event, m.Properties, c.want); err != nil {
			return err
		}
	}
	return nil
}
