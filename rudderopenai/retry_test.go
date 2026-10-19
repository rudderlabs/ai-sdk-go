package rudderopenai_test

import (
	"context"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

// A retryable failure the SDK retries at once.
const throttledReply = `{"status":503,"headers":{"retry-after-ms":"1"},"json":{"error":{"message":"overloaded","type":"server_error"}}}`

const retriedAnswer = `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}}`

func retryingClient(llm *testkit.LLM) openai.Client {
	return openai.NewClient(option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(2),
		option.WithUnsafeAllowHTTP(), rudderopenai.WithTracking())
}

func retryPrices() rudderstackai.ConfigOption {
	return rudderstackai.WithPrices(map[string]rudderstackai.ModelPrice{"gpt-4.1-mini": {TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 2}}})
}

// Node.js and Python wrap the SDK method: a retried call is one call, priced once.
func TestRetryThenSuccessIsOneCall(t *testing.T) {
	for name, inside := range map[string]bool{"interaction": true, "no interaction": false} {
		t.Run(name, func(t *testing.T) {
			rec, llm := setup(t, retryPrices())
			client := retryingClient(llm)
			llm.Script("openai.chat", reply(t, throttledReply), reply(t, throttledReply), reply(t, retriedAnswer))
			call := func(ctx context.Context) error {
				_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
				return err
			}
			var err error
			if inside {
				err = inInteraction(t, call)
			} else {
				err = call(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(llm.Requests("openai.chat")); got != 3 {
				t.Fatalf("requests = %d, want 3", got)
			}
			events := rec.events(t)
			if c := one(t, events, "ai_llm_response_received"); c["error_type"] != nil {
				t.Errorf("call: %v", c)
			}
			p := one(t, events, "ai_response_received")
			breakdown := p["usage_breakdown"].([]any)[0].(obj)
			if p["response_status"] != "success" || num(p["llm_call_count"]) != 1 || num(p["total_cost_usd"]) != 2 ||
				num(breakdown["call_count"]) != 1 || num(breakdown["error_count"]) != 0 {
				t.Errorf("ai_response_received: %v", p)
			}
		})
	}
}

func TestRetriesUsedUpIsOneFailedCall(t *testing.T) {
	for name, inside := range map[string]bool{"interaction": true, "no interaction": false} {
		t.Run(name, func(t *testing.T) {
			rec, llm := setup(t)
			client := retryingClient(llm)
			llm.Script("openai.chat", reply(t, throttledReply), reply(t, throttledReply), reply(t, throttledReply))
			call := func(ctx context.Context) error {
				_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
				return err
			}
			var err error
			if inside {
				err = inInteraction(t, call)
			} else {
				err = call(context.Background())
			}
			if err == nil {
				t.Fatal("want the 503")
			}
			if got := len(llm.Requests("openai.chat")); got != 3 {
				t.Fatalf("requests = %d, want 3", got)
			}
			events := rec.events(t)
			one(t, events, "ai_llm_response_received")
			p := one(t, events, "ai_response_received")
			if p["response_status"] != "error" || p["error_type"] != "api_error" || num(p["llm_call_count"]) != 1 {
				t.Errorf("ai_response_received: %v", p)
			}
		})
	}
}

// A call after a caught failure is a new call: the failure stays recorded, in order.
func TestFailedCallThenNewCall(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, throttledReply), reply(t, retriedAnswer))
	err := inInteraction(t, func(ctx context.Context) error {
		_, _ = client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := one(t, rec.events(t), "ai_response_received")
	if p["response_status"] != "success" || num(p["llm_call_count"]) != 2 {
		t.Errorf("ai_response_received: %v", p)
	}
}
