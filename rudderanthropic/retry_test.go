package rudderanthropic_test

import (
	"context"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

// An overload the SDK retries at once.
var overloaded = testkit.Reply{Status: 529, Headers: map[string]string{"Retry-After": "", "retry-after-ms": "1"},
	JSON: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)}

// Node.js and Python wrap messages.create: a retried call is one call, priced once
// (Node examples/provider-retries).
func TestRetryThenSuccessIsOneCall(t *testing.T) {
	llm, rec := setup(t, rudderstackai.WithLLMCallEvents(true), rudderstackai.WithPrices(map[string]rudderstackai.ModelPrice{
		"claude-haiku-4-5": {TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 1, OutputPerMtok: 5}},
	}))
	llm.Script("anthropic.messages", overloaded, jsonReply(t, map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-haiku-4-5",
		"content": []any{map[string]any{"type": "text", "text": "Hello"}}, "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": 1_000_000, "output_tokens": 0},
	}))
	c := anthropic.NewClient(option.WithBaseURL(llm.URL), option.WithAPIKey("sk-ant-test"), option.WithMaxRetries(2), rudderanthropic.WithTracking())
	err := run(t, func(ctx context.Context) error {
		_, err := c.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, request(nil)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(llm.Requests("anthropic.messages")); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	events := rec.events(t)
	if got := len(events["ai_llm_response_received"]); got != 1 {
		t.Fatalf("ai_llm_response_received: %d", got)
	}
	p := events["ai_response_received"][0]
	eq(t, p, "response_status", "success")
	eq(t, p, "llm_call_count", 1)
	eq(t, p, "total_cost_usd", 1)
}

func TestRetriesUsedUpIsOneFailedCall(t *testing.T) {
	llm, rec := setup(t)
	llm.Script("anthropic.messages", overloaded, overloaded)
	c := anthropic.NewClient(option.WithBaseURL(llm.URL), option.WithAPIKey("sk-ant-test"), option.WithMaxRetries(1), rudderanthropic.WithTracking())
	if _, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{}, body(t, request(nil))); err == nil {
		t.Fatal("want the 529")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("ai_response_received: %d", len(got))
	}
	eq(t, got[0], "response_status", "error")
	eq(t, got[0], "llm_call_count", 1)
}

// The caller's context ends while the SDK waits to retry: one cancelled call.
func TestRetryWaitCancelled(t *testing.T) {
	llm, rec := setup(t)
	slow := overloaded
	slow.Headers = map[string]string{"Retry-After": "", "retry-after-ms": "5000"}
	llm.Script("anthropic.messages", slow)
	c := anthropic.NewClient(option.WithBaseURL(llm.URL), option.WithAPIKey("sk-ant-test"), option.WithMaxRetries(1), rudderanthropic.WithTracking())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, request(nil)))
		done <- err
	}()
	for len(llm.Requests("anthropic.messages")) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("want the cancellation")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("ai_response_received: %d", len(got))
	}
	eq(t, got[0], "response_status", "cancelled")
	eq(t, got[0], "llm_call_count", 1)
}
