package rudderopenai_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
			rec, llm := setup(t, retryPrices(), rudderstackai.WithEmitSpans(true))
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
			span := one(t, events, "ai_otel_span")
			callEvent := one(t, events, "ai_llm_response_received")
			if span["span_id"] != callEvent["span_id"] {
				t.Fatal("retry span join lost")
			}
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

func TestRawResponseEmitSpan(t *testing.T) {
	rec, llm := setup(t, rudderstackai.WithEmitSpans(true))
	llm.Script("openai.chat", reply(t, retriedAnswer))
	client := retryingClient(llm)
	var raw *http.Response
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{}, body(chatBody), option.WithResponseInto(&raw))
	if err != nil {
		t.Fatal(err)
	}
	if raw == nil || raw.StatusCode != 200 {
		t.Fatal("raw response missing")
	}
	events := rec.events(t)
	span := one(t, events, "ai_otel_span")
	call := one(t, events, "ai_llm_response_received")
	if span["span_id"] != call["span_id"] {
		t.Fatal("raw-response join lost")
	}
}

func TestConcurrentIdenticalSDKRetriesKeepInvocationSpans(t *testing.T) {
	rec, _ := setup(t, rudderstackai.WithEmitSpans(true))
	var started atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Header.Get("X-Stainless-Retry-Count") == "0" {
			if started.Add(1) == 2 {
				close(ready)
			}
			<-ready
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"same-response","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL+"/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(1), option.WithUnsafeAllowHTTP(), rudderopenai.WithTracking())
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{}, body(chatBody))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	spans, calls := []obj{}, []obj{}
	for _, event := range rec.events(t) {
		if event.name == "ai_otel_span" {
			spans = append(spans, event.props)
		}
		if event.name == "ai_llm_response_received" {
			calls = append(calls, event.props)
		}
	}
	if len(spans) != 2 || len(calls) != 2 {
		t.Fatalf("spans=%d calls=%d", len(spans), len(calls))
	}
	for _, call := range calls {
		found := false
		for _, span := range spans {
			if call["span_id"] == span["span_id"] && call["interaction_id"] == span["interaction_id"] {
				found = true
			}
		}
		if !found {
			t.Fatal("call joined another invocation's span")
		}
	}
}
