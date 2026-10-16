package rudderopenai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

type obj = map[string]any

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

// events flushes, schema-checks every event and returns them as name + JSON properties.
func (r *recorder) events(t *testing.T) []event {
	t.Helper()
	rudderstackai.Flush(5 * time.Second)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event, 0, len(r.msgs))
	for _, m := range r.msgs {
		raw, _ := json.Marshal(map[string]any(m.Properties))
		var props obj
		_ = json.Unmarshal(raw, &props)
		if err := testkit.ValidateEvent(m.Event, props); err != nil {
			t.Errorf("schema: %s: %v", m.Event, err)
		}
		out = append(out, event{m.Event, props})
	}
	return out
}

type event struct {
	name  string
	props obj
}

// one returns the only event with that name.
func one(t *testing.T, events []event, name string) obj {
	t.Helper()
	var found []obj
	for _, e := range events {
		if e.name == name {
			found = append(found, e.props)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s, got %d: %v", name, len(found), events)
	}
	return found[0]
}

func setup(t *testing.T, opts ...rudderstackai.ConfigOption) (*recorder, *testkit.LLM) {
	t.Helper()
	hook.Reset()
	rec := &recorder{}
	if err := rudderstackai.Configure(append([]rudderstackai.ConfigOption{rudderstackai.WithAnalytics(rec), rudderstackai.WithLLMCallEvents(true)}, opts...)...); err != nil {
		t.Fatal(err)
	}
	llm := testkit.StartLLM()
	t.Cleanup(llm.Close)
	return rec, llm
}

func newClient(llm *testkit.LLM, opts ...rudderopenai.Option) openai.Client {
	return openai.NewClient(option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(0),
		option.WithUnsafeAllowHTTP(), rudderopenai.WithTracking(opts...))
}

func reply(t *testing.T, s string) testkit.Reply {
	t.Helper()
	var r testkit.Reply
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func body(s string) option.RequestOption {
	return option.WithRequestBody("application/json", []byte(s))
}

func inInteraction(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, _ *rudderstackai.Interaction) error {
		return fn(ctx)
	})
}

func num(v any) float64 { f, _ := v.(float64); return f }

const chatBody = `{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}]}`

func TestChatResponse(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm, rudderopenai.WithProvider("litellm"))
	llm.Script("openai.chat", reply(t, `{"headers":{"x-litellm-response-cost":"0.5","x-litellm-model-name":"openai/gpt-4.1-mini-2025"},"json":{
		"id":"c1","model":"alias","service_tier":"priority",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"x","tool_calls":[{"id":"t1","type":"function","function":{"name":"lookup","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"},
			{"index":1,"message":{"role":"assistant","tool_calls":[{"id":"t2","type":"custom","custom":{"name":"grep","input":"raw"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}}}`))
	err := inInteraction(t, func(ctx context.Context) error {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	events := rec.events(t)
	call := one(t, events, "ai_llm_response_received")
	if call["model"] != "openai/gpt-4.1-mini-2025" || call["provider"] != "openai" || num(call["cost_usd"]) != 0.5 ||
		num(call["reasoning_tokens"]) != 2 || num(call["cached_input_tokens"]) != 4 || call["finish_reason"] != "tool_calls" || call["service_tier"] != "fast" {
		t.Errorf("call: %v", call)
	}
	if got := one(t, events, "ai_response_received")["tool_call_count"]; num(got) != 2 {
		t.Errorf("tool_call_count %v", got)
	}
}

func TestChatRefusalAndUnknownUsage(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","refusal":"no"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":"0.25"}}}`),
		reply(t, `{"json":{"id":"c2","model":"gpt-4.1-mini","choices":[]}}`))
	_ = inInteraction(t, func(ctx context.Context) error {
		_, _ = client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		return err
	})
	var calls []obj
	for _, e := range rec.events(t) {
		if e.name == "ai_llm_response_received" {
			calls = append(calls, e.props)
		}
	}
	if len(calls) != 2 || calls[0]["finish_reason"] != "refusal" || num(calls[0]["cost_usd"]) != 0.25 || calls[1]["usage_known"] == true {
		t.Errorf("calls: %v", calls)
	}
}

const streamBody = `{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`

const chatSSE = `{"sse":[
	{"data":{"id":"c1","model":"gpt-4.1-mini","service_tier":"default","choices":[{"index":0,"delta":{"content":"Hel"}}]}},
	{"data":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"lookup","arguments":"{\"a\""}}]}}]}},
	{"data":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}},
	{"data":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}},
	{"data":{"id":"c1","model":"gpt-4.1-mini","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"cost":0.1,"completion_tokens_details":{"reasoning_tokens":1}}}},
	{"data":"[DONE]"}]}`

func consume(stream interface {
	Next() bool
	Err() error
	Close() error
}, each func()) error {
	defer func() { _ = stream.Close() }()
	for stream.Next() {
		each()
	}
	return stream.Err()
}

func TestChatStreamHidesInjectedUsage(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, chatSSE))
	var chunks []openai.ChatCompletionChunk
	err := inInteraction(t, func(ctx context.Context) error {
		s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, body(streamBody))
		return consume(s, func() { chunks = append(chunks, s.Current()) })
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if len(c.Choices) == 0 {
			t.Errorf("the usage chunk we asked for reached the app")
		}
	}
	sent := llm.Requests("openai.chat")
	if len(chunks) != 4 || len(sent) != 1 || !strings.Contains(string(sent[0]), `"include_usage":true`) {
		t.Fatalf("chunks %d, sent %s", len(chunks), sent)
	}
	events := rec.events(t)
	call := one(t, events, "ai_llm_response_received")
	if num(call["input_tokens"]) != 7 || num(call["cost_usd"]) != 0.1 || call["finish_reason"] != "tool_calls" || call["service_tier"] != "standard" {
		t.Errorf("call: %v", call)
	}
	answer := one(t, events, "ai_response_received")
	if num(answer["tool_call_count"]) != 1 || answer["time_to_first_token_ms"] == nil {
		t.Errorf("answer: %v", answer)
	}
}

func TestChatStreamCallerUsageIsShown(t *testing.T) {
	_, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, chatSSE))
	usage := 0
	err := inInteraction(t, func(ctx context.Context) error {
		s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{},
			body(`{"model":"gpt-4.1-mini","messages":[],"stream":true,"stream_options":{"include_usage":true}}`))
		return consume(s, func() {
			if len(s.Current().Choices) == 0 {
				usage++
			}
		})
	})
	if err != nil || usage != 1 {
		t.Fatalf("err %v, usage chunks %d", err, usage)
	}
}

func TestChatStreamUsageRejectedOnce(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat",
		reply(t, `{"status":400,"json":{"error":{"message":"Unrecognized request argument supplied: stream_options","type":"invalid_request_error","code":null}}}`),
		reply(t, chatSSE), reply(t, chatSSE))
	err := inInteraction(t, func(ctx context.Context) error {
		for range 2 {
			s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, body(streamBody))
			if err := consume(s, func() {}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := llm.Requests("openai.chat")
	if len(sent) != 3 || strings.Contains(string(sent[1]), "stream_options") || strings.Contains(string(sent[2]), "stream_options") {
		t.Fatalf("sent %s", sent)
	}
	if n := num(one(t, rec.events(t), "ai_response_received")["llm_call_count"]); n != 1 {
		// both streams share the response id c1, so the second is a repeat
		t.Errorf("llm_call_count %v", n)
	}
}

func TestChatStreamErrorEvent(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, `{"sse":[
		{"data":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"delta":{"content":"a"}}]}},
		{"data":{"error":{"message":"quota","code":"insufficient_quota"}}}]}`))
	err := inInteraction(t, func(ctx context.Context) error {
		s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, body(streamBody))
		return consume(s, func() {})
	})
	if rudderstackai.ErrorTypeOf(err) != "rate_limit" {
		t.Fatalf("err %v -> %s", err, rudderstackai.ErrorTypeOf(err))
	}
	answer := one(t, rec.events(t), "ai_response_received")
	if answer["error_type"] != "rate_limit" || answer["response_status"] != "error" {
		t.Errorf("answer: %v", answer)
	}
}

func TestChatStreamBreak(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, chatSSE))
	_ = inInteraction(t, func(ctx context.Context) error {
		s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, body(streamBody))
		s.Next()
		return s.Close()
	})
	if got := one(t, rec.events(t), "ai_response_received"); got["response_status"] != "cancelled" {
		t.Errorf("answer: %v", got)
	}
}

func TestHTTPErrorsAndClassifier(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.chat", reply(t, `{"status":500,"json":{"error":{"message":"quota","type":"insufficient_quota","code":"insufficient_quota"}}}`))
	err := inInteraction(t, func(ctx context.Context) error {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		return err
	})
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || rudderstackai.ErrorTypeOf(err) != "rate_limit" {
		t.Fatalf("err %v", err)
	}
	if got := one(t, rec.events(t), "ai_llm_response_received"); got["error_type"] != "rate_limit" {
		t.Errorf("call: %v", got)
	}
	if got := rudderstackai.ErrorTypeOf(fmt.Errorf("wrapped: %w", &openai.Error{StatusCode: 503})); got != "api_error" {
		t.Errorf("503 -> %s", got)
	}
}

func TestToolResultsSettleCalls(t *testing.T) {
	rec, llm := setup(t, rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"lookup": {Event: "order_looked_up"}, "web": {Event: "web_called"},
	}}))
	client := newClient(llm)
	llm.Script("openai.chat",
		reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"tool_calls":[{"id":"t1","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{}}}`),
		reply(t, `{"json":{"id":"c2","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"content":"done"},"finish_reason":"stop"}],"usage":{}}}`))
	llm.Script("openai.responses",
		reply(t, `{"json":{"id":"r1","model":"gpt-4.1-mini","status":"completed","output":[{"type":"function_call","call_id":"f1","name":"web","arguments":"{}"}],"usage":{}}}`),
		reply(t, `{"json":{"id":"r2","model":"gpt-4.1-mini","status":"completed","output":[],"usage":{}}}`))
	var beforeEnd []string
	err := inInteraction(t, func(ctx context.Context) error {
		_, _ = client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		_, _ = client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(`{"model":"gpt-4.1-mini","messages":[{"role":"tool","tool_call_id":"t1","content":"{\"ok\":true}"}]}`))
		_, _ = client.Responses.New(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":"hi"}`))
		_, err := client.Responses.New(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":[{"type":"function_call_output","call_id":"f1","output":"ok"}]}`))
		for _, e := range rec.events(t) {
			beforeEnd = append(beforeEnd, e.name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(beforeEnd, ",")
	if !strings.Contains(joined, "order_looked_up") || !strings.Contains(joined, "web_called") {
		t.Errorf("tool events not sent on settle: %s", joined)
	}
}

func TestResponses(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.responses",
		reply(t, `{"json":{"id":"r1","model":"gpt-4.1-mini","status":"completed","service_tier":"flex","output":[
			{"type":"web_search_call","id":"ws1","status":"completed"},
			{"type":"mcp_call","id":"m1","name":"search","server_label":"docs","arguments":"{\"q\":1}"},
			{"type":"custom_tool_call","call_id":"ct1","name":"grep","input":"raw"},
			{"type":"message","content":[{"type":"output_text","text":"answer"}]}],
			"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1},"cost":0.2}}}`),
		reply(t, `{"json":{"id":"r2","model":"gpt-4.1-mini","status":"failed","error":{"code":"rate_limit_exceeded"},"output":[],"usage":{"input_tokens":3,"output_tokens":0}}}`),
		reply(t, `{"json":{"id":"r3","model":"gpt-4.1-mini","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":3,"output_tokens":5}}}`),
		reply(t, `{"json":{"id":"r4","model":"gpt-4.1-mini","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`),
		reply(t, `{"json":{"id":"r5","model":"gpt-4.1-mini","status":"cancelled","output":[],"usage":{"input_tokens":1,"output_tokens":1,"cost":1}}}`),
		reply(t, `{"json":{"id":"r6","model":"gpt-4.1-mini","status":"in_progress","output":[]}}`),
		reply(t, `{"json":{"id":"r7","model":"gpt-4.1-mini","status":"queued","output":[]}}`))
	_ = inInteraction(t, func(ctx context.Context) error {
		for _, b := range []string{`{"model":"gpt-4.1-mini","input":"hi"}`, `{"model":"gpt-4.1-mini","input":"hi"}`, `{"model":"gpt-4.1-mini","input":"hi"}`,
			`{"model":"gpt-4.1-mini","input":"hi"}`, `{"model":"gpt-4.1-mini","input":"hi"}`, `{"model":"gpt-4.1-mini","input":"hi"}`,
			`{"model":"gpt-4.1-mini","input":"hi","background":true}`} {
			_, _ = client.Responses.New(ctx, responses.ResponseNewParams{}, body(b))
		}
		return nil
	})
	events := rec.events(t)
	var calls []obj
	for _, e := range events {
		if e.name == "ai_llm_response_received" {
			calls = append(calls, e.props)
		}
	}
	if len(calls) != 5 {
		t.Fatalf("want 5 calls (queued and background untracked), got %d: %v", len(calls), calls)
	}
	want := []struct{ finish, errorType string }{{"stop", ""}, {"failed", "rate_limit"}, {"length", ""}, {"refusal", ""}, {"cancelled", "cancelled"}}
	for n, w := range want {
		if calls[n]["finish_reason"] != w.finish || (calls[n]["error_type"] != nil && calls[n]["error_type"] != w.errorType) || (w.errorType != "" && calls[n]["error_type"] != w.errorType) {
			t.Errorf("call %d: %v, want %v", n, calls[n], w)
		}
	}
	if calls[0]["service_tier"] != "flex" || num(calls[0]["cost_usd"]) != 0.2 || num(calls[0]["server_tool_call_count"]) != 1 {
		t.Errorf("first call: %v", calls[0])
	}
	if calls[4]["cost_usd"] != nil {
		t.Errorf("cancelled call priced: %v", calls[4])
	}
	if n := num(one(t, events, "ai_response_received")["tool_call_count"]); n != 3 {
		t.Errorf("tool_call_count %v", n)
	}
}

func TestResponsesToolCallsOnly(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.responses", reply(t, `{"json":{"id":"r1","model":"gpt-4.1-mini","status":"completed","output":[{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":""}],"usage":{"input_tokens":1,"output_tokens":1}}}`))
	_, err := client.Responses.New(context.Background(), responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Outside an interaction: its own answer.
	if got := one(t, rec.events(t), "ai_response_received"); got["finish_reason"] != "tool_calls" || got["trigger"] != "system" {
		t.Errorf("answer: %v", got)
	}
}

const responsesStart = `{"event":"response.created","data":{"type":"response.created","response":{"id":"r1","model":"gpt-4.1-mini","status":"in_progress","output":[]}}},
	{"event":"response.output_item.added","data":{"type":"response.output_item.added","item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":""}}},
	{"event":"response.output_text.delta","data":{"type":"response.output_text.delta","delta":"Hi"}}`

func TestResponsesStream(t *testing.T) {
	cases := []struct {
		name, tail, status, errorType string
		calls                         float64
	}{
		{"completed", `,{"event":"response.completed","data":{"type":"response.completed","response":{"id":"r1","model":"gpt-4.1-mini-2025","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},{"type":"message","content":[{"type":"output_text","text":"Hi"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}}`, "success", "", 1},
		{"failed", `,{"event":"response.failed","data":{"type":"response.failed","response":{"id":"r1","model":"gpt-4.1-mini","status":"failed","error":{"code":"server_error"},"output":[],"usage":{"input_tokens":5,"output_tokens":0}}}}`, "error", "api_error", 0},
		{"no terminal", ``, "error", "api_error", 1},
		{"error event", `,{"event":"error","data":{"type":"error","error":{"type":"insufficient_quota","code":"insufficient_quota"}}}`, "error", "rate_limit", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, llm := setup(t)
			client := newClient(llm)
			llm.Script("openai.responses", reply(t, `{"sse":[`+responsesStart+tc.tail+`]}`))
			_ = inInteraction(t, func(ctx context.Context) error {
				s := client.Responses.NewStreaming(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":"hi","stream":true}`))
				_ = consume(s, func() {})
				return nil
			})
			answer := one(t, rec.events(t), "ai_response_received")
			if answer["response_status"] != tc.status || (tc.errorType != "" && answer["error_type"] != tc.errorType) || num(answer["tool_call_count"]) != tc.calls {
				t.Errorf("answer: %v", answer)
			}
		})
	}
}

func TestResponsesStreamLeftOpenAfterTerminal(t *testing.T) {
	rec, llm := setup(t)
	client := newClient(llm)
	llm.Script("openai.responses", reply(t, `{"hang_after":true,"sse":[`+responsesStart+`,{"event":"response.completed","data":{"type":"response.completed","response":{"id":"r1","model":"gpt-4.1-mini","status":"completed","output":[{"type":"message","content":[]}],"usage":{"input_tokens":5,"output_tokens":2}}}}]}`))
	var s interface{ Close() error }
	_ = inInteraction(t, func(ctx context.Context) error {
		st := client.Responses.NewStreaming(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":"hi","stream":true}`))
		s = st
		for st.Next() {
			if st.Current().Type == "response.completed" {
				break
			}
		}
		return nil
	})
	defer func() { _ = s.Close() }()
	call := one(t, rec.events(t), "ai_llm_response_received")
	if call["finish_reason"] != "stop" || num(call["input_tokens"]) != 5 {
		t.Errorf("call: %v", call)
	}
}

func TestBackgroundStreamTrackedAndOtherPathsPassThrough(t *testing.T) {
	rec, llm := setup(t)
	other := &recorder{}
	client := newClient(llm, rudderopenai.WithAnalytics(other), rudderopenai.WithProvider(""))
	llm.Script("openai.responses", reply(t, `{"sse":[`+responsesStart+`,{"event":"response.completed","data":{"type":"response.completed","response":{"id":"r1","model":"gpt-4.1-mini","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}}]}`))
	s := client.Responses.NewStreaming(context.Background(), responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":"hi","stream":true,"background":true}`))
	if err := consume(s, func() {}); err != nil {
		t.Fatal(err)
	}
	// Not a tracked route: passes through untouched (the mock has no route for it).
	if _, err := client.Models.List(context.Background()); err == nil {
		t.Error("want the mock's 404")
	}
	if got := one(t, other.events(t), "ai_response_received"); got["primary_provider"] != "openai" {
		t.Errorf("answer on the integration's client: %v", got)
	}
	if got := rec.events(t); len(got) != 0 {
		t.Errorf("events on the global client: %v", got)
	}
}

func TestClassifierClient(t *testing.T) {
	_, llm := setup(t)
	client := newClient(llm)
	rec := &recorder{}
	hook.Reset()
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithCaptureContent(true), rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{
		Client: rudderopenai.ClassifierClient(client, "gpt-4.1-nano"), Intents: []string{"refund"},
	})); err != nil {
		t.Fatal(err)
	}
	llm.Script("openai.chat",
		reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"content":"Your refund is on its way."},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}}`),
		reply(t, `{"json":{"id":"k1","model":"gpt-4.1-nano","choices":[{"index":0,"message":{"content":"{\"intent\":\"refund\",\"outcome\":\"resolved\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}}`))
	_ = rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1", PromptText: "Where is my refund?"}, func(ctx context.Context, _ *rudderstackai.Interaction) error {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
		return err
	})
	events := rec.events(t)
	classified := one(t, events, "ai_interaction_classified")
	if classified["intent"] != "refund" || classified["classifier_model"] != "gpt-4.1-nano" {
		t.Errorf("classified: %v", classified)
	}
	if n := num(one(t, events, "ai_response_received")["llm_call_count"]); n != 1 {
		t.Errorf("the classifier call was tracked: %v", n)
	}
	sent := llm.Requests("openai.chat")
	if len(sent) != 2 || !strings.Contains(string(sent[1]), `"json_object"`) || !strings.Contains(string(sent[1]), `"max_completion_tokens":300`) {
		t.Errorf("classifier request: %s", sent)
	}
	llm.Script("openai.chat", reply(t, `{"status":400,"json":{"error":{"message":"bad","code":"x"}}}`))
	if _, err := rudderopenai.ClassifierClient(client, "m").Classify(context.Background(), "s", "u"); err == nil {
		t.Error("want the error")
	}
}
