package rudderanthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

type recorder struct {
	mu   sync.Mutex
	msgs []analytics.Track
}

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected %T", m)
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, t)
	r.mu.Unlock()
	return nil
}

func (r *recorder) Close() error { return nil }

// events flushes and returns every event's properties by name, schema-checked.
func (r *recorder) events(t *testing.T) map[string][]map[string]any {
	t.Helper()
	rudderstackai.Flush(5 * time.Second)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]map[string]any{}
	for _, m := range r.msgs {
		raw, _ := json.Marshal(m.Properties)
		var props map[string]any
		_ = json.Unmarshal(raw, &props)
		if err := testkit.ValidateEvent(m.Event, props); err != nil {
			t.Errorf("schema: %v", err)
		}
		out[m.Event] = append(out[m.Event], props)
	}
	return out
}

func setup(t *testing.T, config ...rudderstackai.ConfigOption) (*testkit.LLM, *recorder) {
	t.Helper()
	hook.Reset()
	rec := &recorder{}
	opts := append([]rudderstackai.ConfigOption{rudderstackai.WithAnalytics(rec)}, config...)
	if err := rudderstackai.Configure(opts...); err != nil {
		t.Fatal(err)
	}
	llm := testkit.StartLLM()
	t.Cleanup(llm.Close)
	return llm, rec
}

func newClient(llm *testkit.LLM, opts ...rudderanthropic.Option) anthropic.Client {
	return anthropic.NewClient(option.WithBaseURL(llm.URL), option.WithAPIKey("sk-ant-test"), option.WithMaxRetries(0), rudderanthropic.WithTracking(opts...))
}

func jsonReply(t *testing.T, v any) testkit.Reply {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return testkit.Reply{JSON: raw}
}

func sse(t *testing.T, events ...map[string]any) testkit.Reply {
	t.Helper()
	r := testkit.Reply{}
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		r.SSE = append(r.SSE, testkit.SSEEvent{Event: e["type"].(string), Data: raw})
	}
	return r
}

func body(t *testing.T, v any) option.RequestOption {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return option.WithRequestBody("application/json", raw)
}

func request(extra map[string]any) map[string]any {
	r := map[string]any{"model": "claude-haiku-4-5", "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": "Hi"}}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func eq(t *testing.T, props map[string]any, key string, want any) {
	t.Helper()
	got, _ := json.Marshal(props[key])
	w, _ := json.Marshal(want)
	if string(got) != string(w) {
		t.Errorf("%s = %s, want %s", key, got, w)
	}
}

func run(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, _ *rudderstackai.Interaction) error {
		return fn(ctx)
	})
}

func TestMessageOutsideInteraction(t *testing.T) {
	llm, rec := setup(t)
	llm.Script("anthropic.messages", jsonReply(t, map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-haiku-4-5-20251001",
		"content":     []any{map[string]any{"type": "text", "text": "Hello"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 4, "cache_creation_input_tokens": 2},
	}))
	c := newClient(llm, rudderanthropic.WithProvider("gcp.vertex_ai"))
	if _, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{}, body(t, request(nil))); err != nil {
		t.Fatal(err)
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("ai_response_received: %d", len(got))
	}
	p := got[0]
	eq(t, p, "response_status", "success")
	eq(t, p, "total_input_tokens", 16)
	eq(t, p, "total_output_tokens", 3)
	eq(t, p, "primary_provider", "gcp.vertex_ai")
	eq(t, p, "finish_reason", "end_turn")
}

func TestMessageUsageAndTools(t *testing.T) {
	llm, rec := setup(t, rudderstackai.WithLLMCallEvents(true), rudderstackai.WithToolBreakdown(true))
	llm.Script("anthropic.messages",
		jsonReply(t, map[string]any{
			"id": "msg_1", "model": "claude-haiku-4-5",
			"content": []any{
				map[string]any{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": map[string]any{"query": "x"}},
				map[string]any{"type": "mcp_tool_use", "id": "mcptoolu_1", "name": "search", "server_name": "docs", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"id": 1}},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens": 5, "output_tokens": 7, "cache_creation_input_tokens": 3,
				"cache_creation":        map[string]any{"ephemeral_1h_input_tokens": 2},
				"output_tokens_details": map[string]any{"thinking_tokens": 4},
				"service_tier":          "standard", "speed": "fast",
				"server_tool_use": map[string]any{"web_search_requests": 2, "web_fetch_requests": 0, "other": 1},
			},
		}),
		jsonReply(t, map[string]any{"id": "msg_2", "content": []any{}, "stop_reason": "end_turn"}),
	)
	c := newClient(llm)
	err := run(t, func(ctx context.Context) error {
		if _, err := c.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, request(nil))); err != nil {
			return err
		}
		next := request(map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "Hi"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": true, "content": "boom"},
				map[string]any{"type": "text", "text": "x"},
			}},
		}})
		_, err := c.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, next))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := rec.events(t)
	calls := ev["ai_llm_response_received"]
	if len(calls) != 2 {
		t.Fatalf("ai_llm_response_received: %d", len(calls))
	}
	p := calls[0]
	eq(t, p, "input_tokens", 8)
	eq(t, p, "cache_write_tokens", 3)
	eq(t, p, "reasoning_tokens", 4)
	eq(t, p, "service_tier", "fast")
	eq(t, p, "finish_reason", "tool_use")
	eq(t, p, "model", "claude-haiku-4-5")
	eq(t, calls[1], "model", "claude-haiku-4-5")
	eq(t, p, "server_tool_call_count", 2)
	eq(t, calls[1], "input_tokens", 0)
	r := ev["ai_response_received"][0]
	eq(t, r, "tool_call_count", 3)
	errorsByTool := map[string]any{}
	for _, e := range r["tool_breakdown"].([]any) {
		entry := e.(map[string]any)
		errorsByTool[fmt.Sprint(entry["mcp_server"], "/", entry["tool_name"])] = entry["error_count"]
	}
	eq(t, map[string]any{"e": errorsByTool}, "e", map[string]any{"<nil>/lookup": 1, "<nil>/web_search": nil, "docs/search": nil})
}

func TestRequestedModel(t *testing.T) {
	llm, rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	message := func(model string) testkit.Reply {
		return jsonReply(t, map[string]any{
			"id": "msg_" + model, "type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": "Hi"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 1},
		})
	}
	llm.Script("anthropic.messages", message("claude-haiku-4-5-20251001"), message("claude-x"), sse(t,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_s", "model": "claude-x", "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 1}},
		map[string]any{"type": "message_stop"},
	))
	c := newClient(llm)
	err := run(t, func(ctx context.Context) error {
		for range 2 {
			if _, err := c.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, request(nil))); err != nil {
				return err
			}
		}
		st := c.Messages.NewStreaming(ctx, anthropic.MessageNewParams{}, body(t, request(map[string]any{"stream": true})))
		for st.Next() {
		}
		_ = st.Close()
		return st.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.events(t)["ai_llm_response_received"]
	if len(calls) != 3 {
		t.Fatalf("ai_llm_response_received: %d", len(calls))
	}
	// A dated snapshot of the requested alias is the same model: no requested_model.
	eq(t, calls[0], "requested_model", nil)
	eq(t, calls[1], "requested_model", "claude-haiku-4-5")
	eq(t, calls[2], "requested_model", "claude-haiku-4-5")
}

func TestStream(t *testing.T) {
	llm, rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	llm.Script("anthropic.messages", sse(t,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_s", "model": "claude-haiku-4-5", "usage": map[string]any{"input_tokens": 20, "output_tokens": 1, "cache_read_input_tokens": 5}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Hi"}},
		map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{}}},
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"id":`}},
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `1}`}},
		map[string]any{"type": "content_block_stop", "index": 1},
		map[string]any{"type": "ping"},
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{
			"output_tokens": 30, "cache_creation_input_tokens": 6, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 6},
			"output_tokens_details": map[string]any{"thinking_tokens": 10}, "service_tier": "priority",
			"server_tool_use": map[string]any{"web_search_requests": 1},
		}},
		map[string]any{"type": "message_stop"},
	))
	c := newClient(llm)
	st := c.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{}, body(t, request(map[string]any{"stream": true})))
	for st.Next() {
	}
	if err := st.Err(); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	ev := rec.events(t)
	p := ev["ai_llm_response_received"][0]
	eq(t, p, "input_tokens", 31)
	eq(t, p, "cached_input_tokens", 5)
	eq(t, p, "cache_write_tokens", 6)
	eq(t, p, "output_tokens", 30)
	eq(t, p, "reasoning_tokens", 10)
	eq(t, p, "finish_reason", "tool_use")
	eq(t, p, "response_id", "msg_s")
	eq(t, ev["ai_response_received"][0], "tool_call_count", 1)
	eq(t, ev["ai_response_received"][0], "response_status", "success")
}

func TestStreamErrorEvent(t *testing.T) {
	llm, rec := setup(t)
	llm.Script("anthropic.messages", sse(t,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_e", "model": "claude-haiku-4-5", "usage": map[string]any{"input_tokens": 9, "output_tokens": 2}}},
		map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}},
	))
	c := newClient(llm)
	err := run(t, func(ctx context.Context) error {
		st := c.Messages.NewStreaming(ctx, anthropic.MessageNewParams{}, body(t, request(map[string]any{"stream": true})))
		defer func() { _ = st.Close() }()
		for st.Next() {
		}
		return st.Err()
	})
	if err == nil {
		t.Fatal("expected the stream error")
	}
	if got := rudderstackai.ErrorTypeOf(err); got != "api_error" {
		t.Errorf("ErrorTypeOf = %s", got)
	}
	p := rec.events(t)["ai_response_received"][0]
	eq(t, p, "response_status", "error")
	eq(t, p, "error_type", "api_error")
	eq(t, p, "total_input_tokens", 9)
}

func TestStreamWithoutMessageStop(t *testing.T) {
	llm, rec := setup(t)
	llm.Script("anthropic.messages", sse(t,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_c", "model": "claude-haiku-4-5", "usage": map[string]any{"input_tokens": 9}}},
		map[string]any{"type": "content_block_delta", "index": "x", "delta": map[string]any{"type": "text_delta", "text": "Hi"}},
	))
	c := newClient(llm)
	st := c.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{}, body(t, request(map[string]any{"stream": true})))
	for st.Next() {
	}
	_ = st.Close()
	p := rec.events(t)["ai_response_received"][0]
	eq(t, p, "response_status", "error")
	eq(t, p, "error_type", "api_error")
}

func TestHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
		want   string
	}{{429, "rate_limit_error", "rate_limit"}, {529, "overloaded_error", "api_error"}, {401, "authentication_error", "auth_error"}} {
		t.Run(tc.kind, func(t *testing.T) {
			llm, rec := setup(t)
			raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": tc.kind, "message": "no"}})
			llm.Script("anthropic.messages", testkit.Reply{Status: tc.status, JSON: raw})
			c := newClient(llm)
			_, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{}, body(t, request(nil)))
			var aerr *anthropic.Error
			if !errors.As(err, &aerr) {
				t.Fatalf("err = %v", err)
			}
			if got := rudderstackai.ErrorTypeOf(fmt.Errorf("wrapped: %w", err)); got != tc.want {
				t.Errorf("ErrorTypeOf = %s, want %s", got, tc.want)
			}
			eq(t, rec.events(t)["ai_response_received"][0], "error_type", tc.want)
		})
	}
}

func TestOtherEndpointsUntracked(t *testing.T) {
	llm, rec := setup(t)
	c := newClient(llm)
	_, _ = c.Messages.CountTokens(context.Background(), anthropic.MessageCountTokensParams{}, body(t, request(nil)))
	if n := len(rec.events(t)); n != 0 {
		t.Errorf("events: %d", n)
	}
}

func TestWrappedTwiceRecordsOnce(t *testing.T) {
	llm, _ := setup(t)
	llm.Script("anthropic.messages", jsonReply(t, map[string]any{"id": "msg_1", "model": "claude-haiku-4-5", "content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}))
	own := &recorder{}
	c := anthropic.NewClient(option.WithBaseURL(llm.URL), option.WithAPIKey("k"), option.WithMaxRetries(0),
		option.WithMiddleware(rudderanthropic.Middleware(rudderanthropic.WithAnalytics(own))), rudderanthropic.WithTracking(rudderanthropic.WithAnalytics(own)))
	if _, err := c.Messages.New(context.Background(), anthropic.MessageNewParams{}, body(t, request(nil))); err != nil {
		t.Fatal(err)
	}
	got := own.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("ai_response_received: %d", len(got))
	}
	eq(t, got[0], "llm_call_count", 1)
}

func TestClassifierClient(t *testing.T) {
	llm, _ := setup(t)
	llm.Script("anthropic.messages",
		jsonReply(t, map[string]any{
			"id": "msg_1", "model": "claude-haiku-4-5", "content": []any{map[string]any{"type": "text", "text": `{"intent":"refund"}`}},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1},
		}),
		jsonReply(t, map[string]any{"id": "msg_2", "content": []any{}}),
	)
	cl := rudderanthropic.ClassifierClient(newClient(llm), "claude-haiku-4-5")
	reply, err := cl.Classify(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	want := rudderstackai.ClassifierReply{Text: `{"intent":"refund"}`, Model: "claude-haiku-4-5", Provider: "anthropic", UsageKnown: true, InputTokens: 13, OutputTokens: 5, CachedInputTokens: 2, CacheWriteTokens: 1}
	if reply != want {
		t.Errorf("reply = %+v", reply)
	}
	var sent map[string]any
	_ = json.Unmarshal(llm.Requests("anthropic.messages")[0], &sent)
	if sent["max_tokens"] != float64(300) || sent["system"] == nil {
		t.Errorf("request = %v", sent)
	}
	reply, err = cl.Classify(context.Background(), "sys", "user")
	if err != nil || reply.Model != "claude-haiku-4-5" || reply.UsageKnown {
		t.Errorf("reply = %+v, %v", reply, err)
	}
	if _, err := cl.Classify(context.Background(), "sys", "user"); err == nil {
		t.Error("expected an error with no reply queued")
	}
}
