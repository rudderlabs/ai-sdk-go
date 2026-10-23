package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// runCheck runs the example against the mock LLM server replaying the shared cassette, with
// static credentials.
func runCheck(sink *harness.Sink) error {
	llm, err := harness.MockLLM("bedrock-converse.json")
	if err != nil {
		return err
	}
	defer llm.Close()
	res, err := runSink(sink, aws.Config{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(llm.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "check", SecretAccessKey: "check"}, nil
		}),
		Retryer:    func() aws.Retryer { return aws.NopRetryer{} },
		HTTPClient: testkit.HTTPClient(),
	})
	if err != nil {
		return err
	}
	if n := llm.Pending(); n > 0 {
		return fmt.Errorf("%d cassette replies not served", n)
	}
	if want := "It is 18°C and cloudy in Paris."; res.Answer != want {
		return fmt.Errorf("answer %q, want %q", res.Answer, want)
	}
	return expectEvents(sink.Messages(), res)
}

// expectEvents checks the prompt and the answer, field by field. AWS inputTokens excludes cache
// reads and writes; events count them in: 412 + 1024 written, then 96 + 1024 read.
func expectEvents(msgs []dataplane.Message, res result) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"ai_user_prompt_created": 1, "ai_response_received": 1}); err != nil {
		return err
	}
	checks := []struct {
		event string
		want  map[string]any
	}{
		{"ai_user_prompt_created", map[string]any{"feature": "travel_assistant", "surface": "web"}},
		{"ai_response_received", map[string]any{
			"response_status": "success", "trigger": "user", "primary_provider": "aws.bedrock", "primary_model": model,
			"finish_reason": "stop", "llm_call_count": 2, "tool_call_count": 1,
			"total_input_tokens": 1436 + 1120, "total_output_tokens": 54 + 18, "total_tokens": 2628,
			"total_cached_input_tokens": 1024, "total_cache_write_tokens": 1024,
			// (412 × $1 + 1024 × $1.25 + 54 × $5) + (96 × $1 + 1024 × $0.10 + 18 × $5), per million tokens.
			"total_cost_usd": 0.0022504, "time_to_first_token_ms": harness.Present,
		}},
	}
	for _, c := range checks {
		m := byEvent[c.event][0]
		if got := harness.Integration(m); got != "bedrock" {
			return fmt.Errorf("%s: integration %q, want bedrock", c.event, got)
		}
		if m.UserID != "user_42" {
			return fmt.Errorf("%s: userId %q, want user_42", c.event, m.UserID)
		}
		c.want["interaction_id"], c.want["conversation_id"] = res.InteractionID, "conv_weather_2"
		if err := harness.Fields(c.event, m.Properties, c.want); err != nil {
			return err
		}
	}
	return nil
}
