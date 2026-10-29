package ruddergenai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

const model = "gemini-2.5-flash"

var llm *testkit.LLM

func TestMain(m *testing.M) {
	llm = testkit.StartLLM()
	code := m.Run()
	llm.Close()
	os.Exit(code)
}

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

// events flushes and returns every event, each checked against the spec schema.
func (r *recorder) events(t *testing.T) []analytics.Track {
	t.Helper()
	rudderstackai.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		var props map[string]any
		b, _ := json.Marshal(m.Properties)
		_ = json.Unmarshal(b, &props)
		if err := testkit.ValidateEvent(m.Event, props); err != nil {
			t.Errorf("schema: %v", err)
		}
	}
	return append([]analytics.Track(nil), r.msgs...)
}

// response returns the single ai_response_received, JSON round-tripped.
func (r *recorder) response(t *testing.T) map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range r.events(t) {
		if m.Event == "ai_response_received" {
			var props map[string]any
			b, _ := json.Marshal(m.Properties)
			_ = json.Unmarshal(b, &props)
			out = append(out, props)
		}
	}
	if len(out) != 1 {
		t.Fatalf("expected one ai_response_received, got %d", len(out))
	}
	return out[0]
}

func setup(t *testing.T, opts ...rudderstackai.ConfigOption) *recorder {
	t.Helper()
	hook.Reset()
	llm.Reset()
	rec := &recorder{}
	if err := rudderstackai.Configure(append([]rudderstackai.ConfigOption{rudderstackai.WithAnalytics(rec)}, opts...)...); err != nil {
		t.Fatal(err)
	}
	return rec
}

func newClient(t *testing.T, backend genai.Backend, hc *http.Client) *genai.Client {
	t.Helper()
	c, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: "sk-google-test", Backend: backend, HTTPOptions: genai.HTTPOptions{BaseURL: llm.URL}, HTTPClient: hc,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jsonReply(v any) testkit.Reply {
	b, _ := json.Marshal(v)
	return testkit.Reply{JSON: b}
}

func sseReply(chunks ...any) testkit.Reply {
	r := testkit.Reply{}
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		r.SSE = append(r.SSE, testkit.SSEEvent{Data: b})
	}
	return r
}

type m = map[string]any

func reply(parts []any, usage m, finish string) m {
	cand := m{"content": m{"role": "model", "parts": parts}, "index": 0}
	if finish != "" {
		cand["finishReason"] = finish
	}
	out := m{"candidates": []any{cand}, "modelVersion": model + "-001", "responseId": "r1"}
	if usage != nil {
		out["usageMetadata"] = usage
	}
	return out
}

func interaction(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1", ConversationID: "c1"},
		func(ctx context.Context, _ *rudderstackai.Interaction) error { return fn(ctx) })
}

func num(t *testing.T, props m, key string) float64 {
	t.Helper()
	v, ok := props[key].(float64)
	if !ok {
		t.Fatalf("%s: not a number: %v", key, props[key])
	}
	return v
}

func want(t *testing.T, props m, key string, expected any) {
	t.Helper()
	got, _ := json.Marshal(props[key])
	exp, _ := json.Marshal(expected)
	if string(got) != string(exp) {
		t.Errorf("%s: expected %s, got %s", key, exp, got)
	}
}

func TestGenerateContentGemini(t *testing.T) {
	rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	llm.Script("google.generate", jsonReply(reply(
		[]any{m{"text": "thinking", "thought": true}, m{"text": "Hello"}},
		m{"promptTokenCount": 30, "candidatesTokenCount": 6, "thoughtsTokenCount": 4, "cachedContentTokenCount": 12, "toolUsePromptTokenCount": 5},
		"STOP")))
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	err := interaction(t, func(ctx context.Context) error {
		resp, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
		if err == nil && resp.Text() != "Hello" {
			t.Errorf("text: %q", resp.Text())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := rec.response(t)
	want(t, p, "primary_provider", "gcp.gemini")
	want(t, p, "primary_model", model+"-001")
	want(t, p, "total_input_tokens", 35)
	want(t, p, "total_output_tokens", 10)
	want(t, p, "total_reasoning_tokens", 4)
	want(t, p, "total_cached_input_tokens", 12)
	want(t, p, "finish_reason", "stop")
	calls := 0
	for _, e := range rec.events(t) {
		if e.Event == "ai_llm_response_received" {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("ai_llm_response_received: %d", calls)
	}
}

func TestRequestedModel(t *testing.T) {
	rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	// Gemini serves an alias under another model name.
	served := func(id string) m {
		r := reply([]any{m{"text": "Hi"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 1}, "STOP")
		r["modelVersion"], r["responseId"] = "gemini-3.6-flash", id
		return r
	}
	llm.Script("google.generate", jsonReply(served("r1")))
	llm.Script("google.stream", sseReply(served("r2")))
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	if err := interaction(t, func(ctx context.Context) error {
		if _, err := client.Models.GenerateContent(ctx, "gemini-3.5-flash", genai.Text("Hi"), nil); err != nil {
			return err
		}
		for _, err := range client.Models.GenerateContentStream(ctx, "gemini-3.5-flash", genai.Text("Hi"), nil) {
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var calls []m
	for _, e := range rec.events(t) {
		if e.Event == "ai_llm_response_received" {
			var props m
			b, _ := json.Marshal(e.Properties)
			_ = json.Unmarshal(b, &props)
			calls = append(calls, props)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("ai_llm_response_received: %d", len(calls))
	}
	for _, p := range calls {
		want(t, p, "model", "gemini-3.6-flash")
		want(t, p, "requested_model", "gemini-3.5-flash")
	}
}

func TestVertexExpressAndProviderOverride(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     []ruddergenai.Option
		provider string
	}{
		{"detected", nil, "gcp.vertex_ai"},
		{"override", []ruddergenai.Option{ruddergenai.WithProvider("my.gateway")}, "my.gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := setup(t)
			llm.Script("google.generate", jsonReply(reply([]any{m{"text": "ok"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 1}, "STOP")))
			client := newClient(t, genai.BackendVertexAI, ruddergenai.HTTPClient(nil, tc.opts...))
			if err := interaction(t, func(ctx context.Context) error {
				_, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want(t, rec.response(t), "primary_provider", tc.provider)
		})
	}
}

func TestVertexHost(t *testing.T) {
	rec := setup(t)
	// A tuned model endpoint on an aiplatform host: no publishers path, the host says Vertex.
	rt := ruddergenai.Transport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = "http", llm.URL[len("http://"):]
		req.URL.Path = "/v1beta1/publishers/google/models/x:generateContent"
		return http.DefaultTransport.RoundTrip(req)
	}))
	llm.Script("google.generate", jsonReply(m{"candidates": []any{m{"finishReason": "STOP"}}}))
	if err := interaction(t, func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/l/endpoints/123:generateContent", nil)
		resp, err := rt.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := rec.response(t)
	want(t, p, "primary_provider", "gcp.vertex_ai")
	want(t, p, "primary_model", "123")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStreamLastUsageWins(t *testing.T) {
	rec := setup(t)
	llm.Script("google.stream", sseReply(
		reply([]any{m{"text": "Hel"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 2}, ""),
		reply([]any{m{"text": "lo"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5, "thoughtsTokenCount": 1}, "MAX_TOKENS"),
	))
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	_ = interaction(t, func(ctx context.Context) error {
		for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
			if err != nil {
				return err
			}
		}
		return nil
	})
	p := rec.response(t)
	want(t, p, "total_input_tokens", 10)
	want(t, p, "total_output_tokens", 6)
	want(t, p, "finish_reason", "length")
	want(t, p, "response_status", "incomplete")
	if num(t, p, "time_to_first_token_ms") < 0 {
		t.Error("ttft")
	}
}

func TestStreamBreakCancels(t *testing.T) {
	rec := setup(t)
	llm.Script("google.stream", sseReply(
		reply([]any{m{"text": "Hel"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 2}, ""),
		reply([]any{m{"text": "lo"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5}, "STOP"),
	))
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	_ = interaction(t, func(ctx context.Context) error {
		for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
			if err != nil {
				return err
			}
			break
		}
		return nil
	})
	want(t, rec.response(t), "response_status", "cancelled")
}

// An error line after a chunk with usage fails the call with that usage, as the SDK's APIError.
func TestStreamErrorKeepsUsage(t *testing.T) {
	rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	first, _ := json.Marshal(reply([]any{m{"text": "Hel"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 2, "cachedContentTokenCount": 4}, ""))
	stream := "data: " + string(first) + "\n\n" + `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}` + "\n"
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: r}, nil
	})}
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(hc))
	err := interaction(t, func(ctx context.Context) error {
		for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
			if err != nil {
				return err
			}
		}
		return nil
	})
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected the SDK's APIError, got %v", err)
	}
	var call map[string]any
	for _, e := range rec.events(t) {
		if e.Event == "ai_llm_response_received" {
			b, _ := json.Marshal(e.Properties)
			_ = json.Unmarshal(b, &call)
		}
	}
	want(t, call, "error_type", "rate_limit")
	want(t, call, "input_tokens", 10)
	want(t, call, "output_tokens", 2)
	want(t, call, "cached_input_tokens", 4)
	p := rec.response(t)
	want(t, p, "response_status", "error")
	want(t, p, "error_type", "rate_limit")
	want(t, p, "llm_call_count", 1)
	want(t, p, "total_input_tokens", 10)
	want(t, p, "total_output_tokens", 2)
}

func TestToolLoopSettlesFunctionResponses(t *testing.T) {
	rec := setup(t, rudderstackai.WithToolBreakdown(true))
	second := reply([]any{m{"text": "Sorry."}}, m{"promptTokenCount": 20, "candidatesTokenCount": 2}, "STOP")
	second["responseId"] = "r2"
	llm.Script("google.generate",
		jsonReply(reply([]any{
			m{"functionCall": m{"id": "w1", "name": "get_weather", "args": m{"city": "Paris"}}},
			m{"functionCall": m{"id": "t1", "name": "get_time", "args": m{}}},
		}, m{"promptTokenCount": 10, "candidatesTokenCount": 3}, "STOP")),
		jsonReply(second),
	)
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	err := interaction(t, func(ctx context.Context) error {
		first, err := client.Models.GenerateContent(ctx, model, genai.Text("Weather?"), nil)
		if err != nil {
			return err
		}
		if first.Candidates[0].FinishReason != genai.FinishReasonStop {
			t.Errorf("finish: %v", first.Candidates[0].FinishReason)
		}
		contents := []*genai.Content{
			genai.NewContentFromText("Weather?", genai.RoleUser),
			first.Candidates[0].Content,
			{Role: genai.RoleUser, Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{ID: "w1", Name: "get_weather", Response: map[string]any{"error": "down"}}},
				{FunctionResponse: &genai.FunctionResponse{ID: "t1", Name: "get_time", Response: map[string]any{"output": "12:00"}}},
			}},
		}
		_, err = client.Models.GenerateContent(ctx, model, contents, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := rec.response(t)
	want(t, p, "llm_call_count", 2)
	want(t, p, "tool_call_count", 2)
	want(t, p, "finish_reason", "stop")
	want(t, p, "tool_breakdown", []any{
		m{"tool_name": "get_time", "call_count": 1, "error_count": 0},
		m{"tool_name": "get_weather", "call_count": 1, "error_count": 1, "error_types": m{"api_error": 1}},
	})
}

func TestStreamedPartialArgs(t *testing.T) {
	var got map[string]any
	rules := rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"get_weather": {
		Event: "weather_checked",
		Properties: func(args map[string]any, _ any) map[string]any {
			got = args
			return nil
		},
	}}}
	rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rules))
	llm.Script("google.stream", sseReply(
		reply([]any{m{"functionCall": m{"name": "get_weather", "willContinue": true, "partialArgs": []any{
			m{"jsonPath": "$.city", "stringValue": "Par"},
			m{"jsonPath": "$.opts.days", "numberValue": 3},
			m{"jsonPath": "$.opts.metric", "boolValue": true},
			m{"jsonPath": "$.note", "nullValue": "NULL_VALUE"},
			m{"jsonPath": "bad"},
			m{"jsonPath": "$.a..b", "stringValue": "x"},
			m{"jsonPath": "$.empty"},
		}}}}, m{"promptTokenCount": 4, "candidatesTokenCount": 1}, ""),
		reply([]any{m{"functionCall": m{"partialArgs": []any{m{"jsonPath": "$.city", "stringValue": "is"}}}}},
			m{"promptTokenCount": 4, "candidatesTokenCount": 2}, ""),
		reply([]any{m{"executableCode": m{"code": "1"}}, m{"text": "done"}}, m{"promptTokenCount": 4, "candidatesTokenCount": 3}, "STOP"),
	))
	client := newClient(t, genai.BackendVertexAI, ruddergenai.HTTPClient(nil))
	_ = interaction(t, func(ctx context.Context) error {
		for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
			if err != nil {
				return err
			}
		}
		return nil
	})
	p := rec.response(t)
	want(t, p, "tool_call_count", 2)
	want(t, p, "finish_reason", "tool_calls")
	want(t, got, "city", "Paris")
	want(t, got, "opts", m{"days": 3, "metric": true})
	if v, ok := got["note"]; !ok || v != nil {
		t.Errorf("note: %v %v", v, ok)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name, errorType string
		status          int
		body            m
	}{
		{"429", "rate_limit", 429, m{"error": m{"code": 429, "message": "quota", "status": "RESOURCE_EXHAUSTED"}}},
		{"503 exhausted", "rate_limit", 503, m{"error": m{"code": 503, "message": "quota", "status": "RESOURCE_EXHAUSTED"}}},
		{"500", "api_error", 500, m{"error": m{"code": 500, "message": "boom", "status": "INTERNAL"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := setup(t)
			r := jsonReply(tc.body)
			r.Status = tc.status
			llm.Script("google.generate", r)
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			err := interaction(t, func(ctx context.Context) error {
				_, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
				return err
			})
			var apiErr genai.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.status {
				t.Fatalf("expected the SDK's APIError, got %v", err)
			}
			if got := rudderstackai.ErrorTypeOf(err); got != tc.errorType {
				t.Errorf("ErrorTypeOf: %s", got)
			}
			p := rec.response(t)
			want(t, p, "error_type", tc.errorType)
			want(t, p, "primary_model", model)
		})
	}
}

func TestErrorClassifier(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{genai.APIError{Code: 403}, "auth_error"},
		{&genai.APIError{Code: 400}, "invalid_request"},
		{fmt.Errorf("wrapped: %w", genai.APIError{Code: 499, Status: "CANCELLED"}), "cancelled"},
		{genai.APIError{Code: 500, Status: "DEADLINE_EXCEEDED"}, "timeout"},
		{genai.APIError{Code: 500, Status: "UNAUTHENTICATED"}, "auth_error"},
		{genai.APIError{Code: 503, Status: "UNAVAILABLE"}, "api_error"},
		{(*genai.APIError)(nil), "api_error"},
		{context.DeadlineExceeded, "timeout"},
	} {
		if got := rudderstackai.ErrorTypeOf(tc.err); got != tc.want {
			t.Errorf("%v: expected %s, got %s", tc.err, tc.want, got)
		}
	}
}

func TestOutsideInteractionUsesOwnAnalytics(t *testing.T) {
	setup(t)
	own := &recorder{}
	llm.Script("google.generate", jsonReply(m{"promptFeedback": m{"blockReason": "SAFETY"}, "usageMetadata": m{"promptTokenCount": 9}}))
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil, ruddergenai.WithAnalytics(own)))
	if _, err := client.Models.GenerateContent(context.Background(), model, genai.Text("Hi"), nil); err != nil {
		t.Fatal(err)
	}
	p := own.response(t)
	want(t, p, "finish_reason", "content_filter")
	want(t, p, "error_type", "content_filter")
	want(t, p, "primary_model", model)
}

func TestFinishReasonsAndNoUsage(t *testing.T) {
	for _, tc := range []struct {
		finish, want string
		parts        []any
	}{
		{"RECITATION", "content_filter", []any{}},
		{"MALFORMED_FUNCTION_CALL", "MALFORMED_FUNCTION_CALL", []any{}},
		{"", "", []any{m{"text": "partial"}}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			rec := setup(t, rudderstackai.WithLLMCallEvents(true))
			llm.Script("google.generate", jsonReply(reply(tc.parts, nil, tc.finish)))
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			_ = interaction(t, func(ctx context.Context) error {
				_, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
				return err
			})
			p := rec.response(t)
			if tc.want == "" {
				if _, ok := p["finish_reason"]; ok {
					t.Errorf("finish_reason: %v", p["finish_reason"])
				}
			} else {
				want(t, p, "finish_reason", tc.want)
			}
			if _, ok := p["total_cost_usd"]; ok {
				t.Error("a call without usage is unpriced")
			}
		})
	}
}

func TestWrappedTwiceCountsOnce(t *testing.T) {
	rec := setup(t)
	llm.Script("google.generate", jsonReply(reply([]any{m{"text": "ok"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 1}, "STOP")))
	hc := ruddergenai.HTTPClient(ruddergenai.HTTPClient(&http.Client{Timeout: time.Minute}))
	if hc.Timeout != time.Minute {
		t.Error("base client settings are kept")
	}
	client := newClient(t, genai.BackendGeminiAPI, hc)
	if err := interaction(t, func(ctx context.Context) error {
		_, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want(t, rec.response(t), "llm_call_count", 1)
}

func TestOtherRequestsPassThrough(t *testing.T) {
	rec := setup(t)
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	_ = interaction(t, func(ctx context.Context) error {
		_, err := client.Models.CountTokens(ctx, model, genai.Text("Hi"), nil)
		if err == nil {
			t.Error("the mock has no countTokens route")
		}
		return nil
	})
	p := rec.response(t)
	want(t, p, "llm_call_count", 0)
}

func TestTransportDefaultBase(t *testing.T) {
	rec := setup(t)
	llm.Script("google.generate", jsonReply(reply([]any{m{"text": "ok"}}, m{"promptTokenCount": 1}, "STOP")))
	client := newClient(t, genai.BackendGeminiAPI, &http.Client{Transport: ruddergenai.Transport(nil)})
	if err := interaction(t, func(ctx context.Context) error {
		_, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want(t, rec.response(t), "llm_call_count", 1)
}

func TestContextLimitClassifiers(t *testing.T) {
	message := "The input token count exceeds the maximum number of tokens allowed (32768)."
	for _, err := range []error{genai.APIError{Code: 400, Message: message}, &genai.APIError{Code: 422, Message: message}} {
		if got := rudderstackai.ErrorTypeOf(err); got != "context_limit" {
			t.Fatalf("typed classifier: %s", got)
		}
	}
}
