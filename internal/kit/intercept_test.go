package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// chatStream reads a chat-completions style stream into an Observer.
type chatStream struct {
	*Observer
	mu     sync.Mutex
	events []string
}

func (s *chatStream) OnEvent(event string, data []byte) {
	s.mu.Lock()
	s.events = append(s.events, event+"|"+string(data))
	s.mu.Unlock()
	if string(data) == "[DONE]" {
		s.Done = true
		return
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return
	}
	if id := Str(m["id"]); id != "" {
		s.ResponseID = id
	}
	if u := Map(m["usage"]); u != nil {
		s.InputTokens, s.OutputTokens = IntPtr(u["prompt_tokens"]), IntPtr(u["completion_tokens"])
	}
	for _, c := range List(m["choices"]) {
		choice := Map(c)
		delta := Map(choice["delta"])
		if text := Str(delta["content"]); text != "" {
			s.FirstToken()
			s.NoteText(text)
		}
		for _, tc := range List(delta["tool_calls"]) {
			call := Map(tc)
			key := strconv.Itoa(Int(call["index"]))
			fn := Map(call["function"])
			s.NoteTool(key, Str(call["id"]), Str(fn["name"]), nil, "")
			s.NoteToolArgs(key, Str(fn["arguments"]))
		}
		if fr := Str(choice["finish_reason"]); fr != "" {
			s.FinishReason = fr
		}
	}
}

func (s *chatStream) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// harness is a chat-completions style Spec in front of an httptest server.
type harness struct {
	spec     *Spec
	srv      *httptest.Server
	client   *http.Client
	rec      *recorder
	stream   atomic.Pointer[chatStream]
	accepted atomic.Int32
	bodies   chan map[string]any
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{rec: newRecorder(t), bodies: make(chan map[string]any, 8)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case h.bodies <- body:
		default:
		}
		r.Body = io.NopCloser(strings.NewReader(mustJSON(body)))
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)
	h.spec = &Spec{
		Provider:    "openai",
		Integration: "kit-test",
		Analytics:   h.rec,
		IsStream:    func(_ *http.Request, body map[string]any) bool { return body["stream"] == true },
		PrepareStream: func(c *Call) *Prepared {
			body := map[string]any{}
			for k, v := range c.Request {
				body[k] = v
			}
			body["stream_options"] = map[string]any{"include_usage": true}
			return &Prepared{Body: body, Names: []string{"stream_options"}, Accepted: func() { h.accepted.Add(1) }}
		},
		OnResponse: func(c *Call, body map[string]any, _ *http.Response) bool {
			if Str(body["object"]) != "chat.completion" {
				return false
			}
			u := Map(body["usage"])
			c.Interaction.RecordCall(rudderstackai.LLMCall{
				Provider: c.Provider, Model: Str(body["model"]), RequestedModel: c.Model, Integration: c.Integration,
				InputTokens: Int(u["prompt_tokens"]), OutputTokens: Int(u["completion_tokens"]), ResponseID: Str(body["id"]),
			})
			return true
		},
		NewStream: func(c *Call, _ *http.Response) StreamHandler {
			s := &chatStream{Observer: NewObserver(c)}
			h.stream.Store(s)
			return s
		},
	}
	base := h.srv.Client().Transport
	h.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return h.spec.Intercept(r, base.RoundTrip)
	})}
	return h
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (h *harness) post(ctx context.Context, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return h.client.Do(req)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const completion = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-mini-2024-07-18","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func sseHandler(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return string(b)
}

const chatRequest = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hi"}]}`

func TestInterceptJSONSuccess(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	var seen *Call
	h.spec.OnRequest = func(c *Call) { seen = c }
	resp, err := h.post(context.Background(), chatRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp); got != completion {
		t.Fatalf("response body changed: %s", got)
	}
	if seen == nil || !seen.Ephemeral || seen.Model != "gpt-4o-mini" || seen.Provider != "openai" || seen.HTTPRequest == nil || Str(seen.Request["model"]) != "gpt-4o-mini" {
		t.Fatalf("OnRequest call = %+v", seen)
	}
	if body := <-h.bodies; mustJSON(body) != mustJSON(map[string]any{"model": "gpt-4o-mini", "messages": []any{map[string]any{"role": "user", "content": "Hi"}}}) {
		t.Fatalf("request body changed: %v", body)
	}
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{
		"provider": "openai", "model": "gpt-4o-mini-2024-07-18",
		"input_tokens": 11, "output_tokens": 4, "response_id": "chatcmpl-1", "error_type": nil,
	})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "llm_call_count": 1, "total_input_tokens": 11})
}

func TestInterceptActiveInteraction(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	ctx, i := interaction(h.rec)
	for _, body := range []string{
		chatRequest,
		`{"model":"gpt-4o-mini","input":"Hi"}`,
		`{"model":"gemini-2.0-flash","contents":[{"parts":[{"text":"Hi"}]}]}`,
	} {
		resp, err := h.post(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
	}
	h.rec.none(t, "ai_response_received")
	i.End(rudderstackai.EndOptions{})
	// Same response ID three times: counted once.
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "llm_call_count": 1})
}

func TestInterceptNotRecorded(t *testing.T) {
	cases := map[string]string{
		"not a finished call": `{"object":"response","status":"queued"}`,
		"not json":            `plain text`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, jsonHandler(200, body))
			ctx, i := interaction(h.rec)
			resp, err := h.post(ctx, chatRequest)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, resp); got != body {
				t.Fatalf("body = %q", got)
			}
			i.End(rudderstackai.EndOptions{})
			h.rec.none(t, "ai_llm_response_received")
			want(t, h.rec.props(t, "ai_response_received"), map[string]any{"llm_call_count": 0})
		})
	}
}

func TestInterceptPassThrough(t *testing.T) {
	t.Run("not a POST", func(t *testing.T) {
		h := newHarness(t, jsonHandler(200, `{"data":[]}`))
		resp, err := h.client.Get(h.srv.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		if got := h.rec.names(); len(got) != 0 {
			t.Fatalf("events for a GET: %v", got)
		}
	})
	t.Run("untracked by the spec", func(t *testing.T) {
		h := newHarness(t, jsonHandler(200, completion))
		h.spec.Untracked = func(_ *http.Request, body map[string]any) bool { return body["background"] == true }
		resp, err := h.post(context.Background(), `{"model":"gpt-4o-mini","background":true}`)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		if got := h.rec.names(); len(got) != 0 {
			t.Fatalf("events for an untracked request: %v", got)
		}
	})
	t.Run("no body", func(t *testing.T) {
		h := newHarness(t, jsonHandler(200, completion))
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL, nil)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"requested_model": nil})
	})
	t.Run("hooks panic", func(t *testing.T) {
		h := newHarness(t, jsonHandler(200, completion))
		h.spec.OnRequest = func(*Call) { panic("hook") }
		h.spec.OnResponse = func(*Call, map[string]any, *http.Response) bool { panic("hook") }
		resp, err := h.post(context.Background(), chatRequest)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, resp); got != completion {
			t.Fatalf("body = %q", got)
		}
	})
}

func TestInterceptWrappedTwice(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	inner := h.client.Transport
	outer := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return h.spec.Intercept(r, inner.RoundTrip)
	})}
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL, strings.NewReader(chatRequest))
	resp, err := outer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if got := h.rec.named("ai_llm_response_received"); len(got) != 1 {
		t.Fatalf("want 1 call, got %d: %v", len(got), h.rec.names())
	}
	h.rec.props(t, "ai_response_received")
}

func TestInterceptUntrackedContext(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	type result struct {
		forCall   bool
		untracked bool
		body      string
		err       error
	}
	done := make(chan result, 1)
	if err := rudderstackai.Configure(rudderstackai.WithEvaluator(func(ctx context.Context, _ rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
		var r result
		i, _ := ForCall(ctx, "kit-test", h.rec)
		r.forCall, r.untracked = i != nil, Untracked(ctx)
		resp, err := h.post(ctx, chatRequest)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			r.body = string(b)
		}
		r.err = err
		done <- r
		return nil, nil
	})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithEvaluator(nil)) })
	_, i := interaction(h.rec)
	i.End(rudderstackai.EndOptions{})
	r := <-done
	if r.err != nil || r.body != completion {
		t.Fatalf("evaluator request: %q, %v", r.body, r.err)
	}
	if r.forCall || !r.untracked {
		t.Fatalf("evaluator context: ForCall %v, Untracked %v", r.forCall, r.untracked)
	}
	h.rec.none(t, "ai_llm_response_received")
}

func TestInterceptHTTPError(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		errorType  func(int, map[string]any) string
		want       map[string]any
	}{
		{"rate limit", `{"error":{"message":"slow down"}}`, 429, nil, map[string]any{"response_status": "error", "error_type": "rate_limit"}},
		{"quota on 500", `{"error":{"code":"insufficient_quota"}}`, 500, nil, map[string]any{"error_type": "rate_limit"}},
		{"server error", `oops`, 500, nil, map[string]any{"error_type": "api_error"}},
		{"timeout status", `{}`, 504, nil, map[string]any{"response_status": "timeout", "error_type": "timeout"}},
		{"spec mapping", `{"type":"error","error":{"type":"overloaded_error"}}`, 529,
			func(status int, body map[string]any) string {
				if Str(Map(body["error"])["type"]) == "overloaded_error" {
					return "rate_limit"
				}
				return ""
			}, map[string]any{"error_type": "rate_limit"}},
		{"spec mapping defers", `{}`, 401, func(int, map[string]any) string { return "" }, map[string]any{"error_type": "auth_error"}},
		{"spec mapping panics", `{}`, 403, func(int, map[string]any) string { panic("map") }, map[string]any{"error_type": "auth_error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, jsonHandler(c.status, c.body))
			h.spec.ErrorType = c.errorType
			resp, err := h.post(context.Background(), chatRequest)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != c.status || readAll(t, resp) != c.body {
				t.Fatalf("response changed: %d", resp.StatusCode)
			}
			want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": c.want["error_type"], "input_tokens": 0})
			want(t, h.rec.props(t, "ai_response_received"), c.want)
		})
	}
}

func TestInterceptTransportError(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	url := h.srv.URL
	h.srv.Close()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(chatRequest))
	if _, err := h.client.Do(req); err == nil {
		t.Fatal("want a connection error")
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "error_type": "connection_error"})
}

func TestInterceptCancelled(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.post(ctx, chatRequest); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled", "error_type": "cancelled"})
}

func chunk(v string) string { return "data: " + v + "\n\n" }

var streamEvents = []string{
	": keep-alive\n\n",
	chunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`),
	chunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":" world"}}]}`),
	chunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`),
	chunk(`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`),
	chunk(`{"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":7}}`),
	chunk(`[DONE]`),
}

const streamRequest = `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`

func TestInterceptStreamCompleted(t *testing.T) {
	h := newHarness(t, sseHandler(streamEvents...))
	ctx, i := interaction(h.rec)
	resp, err := h.post(ctx, streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp); got != strings.Join(streamEvents, "") {
		t.Fatalf("stream changed: %q", got)
	}
	if body := <-h.bodies; Map(body["stream_options"])["include_usage"] != true {
		t.Fatalf("stream_options not added: %v", body)
	}
	call := h.rec.props(t, "ai_llm_response_received")
	want(t, call, map[string]any{"input_tokens": 20, "output_tokens": 7, "response_id": "chatcmpl-2", "finish_reason": "tool_calls", "error_type": nil})
	if s := h.stream.Load(); s == nil || !s.HasTools() {
		t.Fatal("tool call not seen")
	}
	i.End(rudderstackai.EndOptions{})
	resp2 := h.rec.props(t, "ai_response_received")
	want(t, resp2, map[string]any{"response_status": "success", "tool_call_count": 1, "llm_call_count": 1})
	if _, ok := resp2["time_to_first_token_ms"]; !ok {
		t.Error("time_to_first_token_ms missing")
	}
}

func TestInterceptStreamEphemeral(t *testing.T) {
	// The last event has no closing blank line and the body no final newline.
	h := newHarness(t, sseHandler("event: message\n", "data: {\"id\":\"c3\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n", "id: 7\n\n", "data: [DONE]"))
	resp, err := h.post(context.Background(), streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	s := h.stream.Load()
	if got := strings.Join(s.seen(), ","); got != `message|{"id":"c3","usage":{"prompt_tokens":1,"completion_tokens":1}},|[DONE]` {
		t.Fatalf("events = %s", got)
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "total_input_tokens": 1})
}

func TestInterceptStreamClosedEarly(t *testing.T) {
	h := newHarness(t, sseHandler(streamEvents...))
	ctx, i := interaction(h.rec)
	resp, err := h.post(ctx, streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": "cancelled", "cost_usd": nil})
	i.End(rudderstackai.EndOptions{})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled"})
}

func TestInterceptStreamClosedAfterDone(t *testing.T) {
	// The terminal event arrives, then the server holds the connection open.
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunk(`{"id":"c4","choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)+chunk(`[DONE]`))
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	resp, err := h.post(context.Background(), streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Body
	buf := make([]byte, 1)
	for !h.stream.Load().Done {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": nil, "input_tokens": 2})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success"})
}

type failingBody struct {
	io.Reader
	closed bool
}

func (b *failingBody) Close() error { b.closed = true; return nil }

func TestInterceptStreamReadError(t *testing.T) {
	cases := []struct {
		name, tail string
		want       map[string]any
	}{
		{"after a full line", "", map[string]any{"error_type": "api_error", "input_tokens": 5}},
		{"inside a line", "data: {\"partial", map[string]any{"error_type": "api_error", "input_tokens": 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, sseHandler(chunk(`{"id":"c5","usage":{"prompt_tokens":5,"completion_tokens":2}}`)))
			body := &failingBody{}
			h.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				return h.spec.Intercept(r, func(r *http.Request) (*http.Response, error) {
					resp, err := h.srv.Client().Transport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					body.Reader = io.MultiReader(resp.Body, strings.NewReader(c.tail), errReader{})
					resp.Body = body
					return resp, nil
				})
			})
			resp, err := h.post(context.Background(), streamRequest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Fatal("want the read error")
			}
			_ = resp.Body.Close()
			want(t, h.rec.props(t, "ai_llm_response_received"), c.want)
			want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "error_type": "api_error"})
			if !body.closed {
				t.Error("source body not closed")
			}
		})
	}
}

// A stream that fails after its usage arrived is a counted, failed call that keeps that usage.
func TestInterceptStreamReadErrorKeepsUsage(t *testing.T) {
	h := newHarness(t, sseHandler(chunk(`{"id":"c7","choices":[{"index":0,"delta":{"content":"Hi"}}]}`), chunk(`{"id":"c7","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4}}`)))
	base := h.srv.Client().Transport
	h.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return h.spec.Intercept(r, func(r *http.Request) (*http.Response, error) {
			resp, err := base.RoundTrip(r)
			if err != nil {
				return nil, err
			}
			resp.Body = &failingBody{Reader: io.MultiReader(resp.Body, errReader{})}
			return resp, nil
		})
	})
	ctx, i := interaction(h.rec)
	resp, err := h.post(ctx, streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("want the read error")
	}
	_ = resp.Body.Close()
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": "api_error", "input_tokens": 12, "output_tokens": 4, "total_tokens": 16})
	i.End(rudderstackai.EndOptions{})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{
		"response_status": "error", "error_type": "api_error", "llm_call_count": 1, "total_input_tokens": 12, "total_output_tokens": 4,
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestInterceptStreamContextCancelled(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunk(`{"id":"c6","choices":[{"delta":{"content":"partial"}}]}`))
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.post(ctx, streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("want an error after cancel")
	}
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": "cancelled"})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled"})
}

func TestInterceptStreamNotEventStream(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	h.spec.PrepareStream = nil
	resp, err := h.post(context.Background(), streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if h.stream.Load() != nil {
		t.Fatal("a JSON answer is not read as a stream")
	}
	want(t, h.rec.props(t, "ai_llm_response_received"), map[string]any{"input_tokens": 11})
}

func TestInterceptStreamNoHandler(t *testing.T) {
	h := newHarness(t, sseHandler(streamEvents...))
	h.spec.NewStream = func(*Call, *http.Response) StreamHandler { return nil }
	ctx, i := interaction(h.rec)
	resp, err := h.post(ctx, streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp); got != strings.Join(streamEvents, "") {
		t.Fatalf("stream changed: %q", got)
	}
	i.End(rudderstackai.EndOptions{})
	h.rec.none(t, "ai_llm_response_received")
}

func TestInterceptPreparedRefused(t *testing.T) {
	cases := []struct {
		name, refusal string
		accepted      int32
	}{
		{"named", `{"error":{"message":"Unrecognized request argument supplied: stream_options","code":null}}`, 1},
		{"unclear", `{"error":{"message":"bad request"}}`, 0},
		{"unclear, not json", `Bad Request`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if _, ok := body["stream_options"]; ok {
					jsonHandler(400, c.refusal)(w, r)
					return
				}
				sseHandler(streamEvents...)(w, r)
			})
			resp, err := h.post(context.Background(), streamRequest)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			readAll(t, resp)
			if calls.Load() != 2 {
				t.Fatalf("want a resend, got %d requests", calls.Load())
			}
			if h.accepted.Load() != c.accepted {
				t.Fatalf("Accepted ran %d times, want %d", h.accepted.Load(), c.accepted)
			}
			want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success"})
		})
	}
}

func TestInterceptPreparedRefusedOther(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		requests   int32
		errorType  string
	}{
		// A coded 400 that names nothing we added is the caller's own error.
		{"coded 400", `{"error":{"message":"bad model","code":"model_not_found"}}`, 400, 1, "invalid_request"},
		{"500", `{}`, 500, 1, "api_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				jsonHandler(c.status, c.body)(w, r)
			})
			resp, err := h.post(context.Background(), streamRequest)
			if err != nil {
				t.Fatal(err)
			}
			readAll(t, resp)
			if calls.Load() != c.requests {
				t.Fatalf("requests = %d, want %d", calls.Load(), c.requests)
			}
			want(t, h.rec.props(t, "ai_response_received"), map[string]any{"error_type": c.errorType})
		})
	}
}

func TestInterceptPreparedResendFails(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			jsonHandler(400, `{"error":{"message":"stream_options is not supported"}}`)(w, r)
			return
		}
		jsonHandler(429, `{"error":{"message":"slow down"}}`)(w, r)
	})
	resp, err := h.post(context.Background(), streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if h.accepted.Load() != 0 {
		t.Fatal("Accepted ran for a failed resend")
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"error_type": "rate_limit"})
}

func TestInterceptPreparedUnencodable(t *testing.T) {
	h := newHarness(t, sseHandler(streamEvents...))
	h.spec.PrepareStream = func(*Call) *Prepared {
		return &Prepared{Body: map[string]any{"bad": func() {}}, Names: []string{"bad"}}
	}
	resp, err := h.post(context.Background(), streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if body := <-h.bodies; body["bad"] != nil || body["stream_options"] != nil {
		t.Fatalf("original request not sent: %v", body)
	}
	h.rec.props(t, "ai_response_received")
}

func TestRefusedParams(t *testing.T) {
	resp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}
	names := []string{"stream_options"}
	cases := []struct {
		resp *http.Response
		want string
	}{
		{resp(422, `{"detail":"stream_options: extra fields not permitted"}`), "named"},
		{resp(400, `{"error":{"code":"x"}}`), ""},
		{resp(400, ``), "unclear"},
		{resp(404, `stream_options`), ""},
		{&http.Response{StatusCode: 400}, "unclear"},
	}
	for n, c := range cases {
		if got := refusedParams(c.resp, names); got != c.want {
			t.Errorf("case %d: got %q, want %q", n, got, c.want)
		}
	}
}

func TestSSEParse(t *testing.T) {
	var got []string
	b := &sseBody{h: &funcHandler{onEvent: func(e string, d []byte) { got = append(got, fmt.Sprintf("%s=%s", e, d)) }}}
	for _, line := range []string{"event: ping\r\n", "\r\n", "data: a\n", "data:b\n", "retry: 10\n", "\n", "\n"} {
		b.parse([]byte(line))
	}
	if strings.Join(got, ";") != "ping=;=a\nb" {
		t.Fatalf("events = %q", got)
	}
}

func TestSSEParseOtherLines(t *testing.T) {
	h := &lineHandler{funcHandler: funcHandler{onEvent: func(string, []byte) {}}}
	b := &sseBody{h: h}
	for _, line := range []string{": comment\n", "id: 1\n", "retry: 5\n", `{"error":{"code":429}}` + "\n"} {
		b.parse([]byte(line))
	}
	if strings.Join(h.lines, ";") != `{"error":{"code":429}}` {
		t.Fatalf("lines = %q", h.lines)
	}
}

type lineHandler struct {
	funcHandler
	lines []string
}

func (l *lineHandler) OnLine(line []byte) { l.lines = append(l.lines, string(line)) }

type funcHandler struct {
	onEvent func(string, []byte)
}

func (f *funcHandler) OnEvent(e string, d []byte) { f.onEvent(e, d) }
func (f *funcHandler) Finish(End, string)         {}
func (f *funcHandler) Complete() bool             { return false }
