package rudderanthropic_test

import (
	"context"
	"os"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

// TestLive makes one real call when ANTHROPIC_API_KEY is set.
func TestLive(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	_, rec := setup(t)
	c := anthropic.NewClient(option.WithMaxRetries(0), rudderanthropic.WithTracking())
	st := c.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{
		Model: "claude-haiku-4-5", MaxTokens: 20,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say hi."))},
	})
	for st.Next() {
	}
	if err := st.Err(); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("ai_response_received: %d", len(got))
	}
	p := got[0]
	t.Logf("status=%v model=%v input=%v output=%v finish=%v", p["response_status"], p["primary_model"], p["total_input_tokens"], p["total_output_tokens"], p["finish_reason"])
	if p["response_status"] != "success" || p["total_input_tokens"] == float64(0) {
		t.Errorf("unexpected: %v", p)
	}
}
