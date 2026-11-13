package rudderopenai_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

type connectionRetryTransport struct {
	attempts int
	failure  error
}

func (rt *connectionRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.attempts++
	if rt.attempts == 1 {
		return nil, rt.failure
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"connection-retry","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)), Request: req}, nil
}
func TestOpenAIConnectionRetriesKeepOneInvocationSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, failure := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, context.DeadlineExceeded} {
			t.Run(fmt.Sprintf("inside=%v/%v", inside, failure), func(t *testing.T) {
				rec, _ := setup(t, rudderstackai.WithEmitSpans(true))
				rt := &connectionRetryTransport{failure: failure}
				client := openai.NewClient(option.WithBaseURL("https://provider.test/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(1), option.WithHTTPClient(&http.Client{Transport: rt}), rudderopenai.WithTracking())
				ctx := context.Background()
				var owner *rudderstackai.Interaction
				if inside {
					ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice", Classify: rudderstackai.Ptr(false)})
					defer owner.End(rudderstackai.EndOptions{})
				}
				_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
				if err != nil {
					t.Fatal(err)
				}
				if owner != nil {
					owner.End(rudderstackai.EndOptions{})
				}
				if rt.attempts != 2 {
					t.Fatalf("attempts=%d", rt.attempts)
				}
				events := rec.events(t)
				span, call := one(t, events, "ai_otel_span"), one(t, events, "ai_llm_response_received")
				if span["span_id"] != call["span_id"] || span["interaction_id"] != call["interaction_id"] || span["status_code"] != "ok" {
					t.Fatal("connection retry lost invocation span")
				}
			})
		}
	}
}
