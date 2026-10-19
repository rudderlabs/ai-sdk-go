package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startLLM(t *testing.T) *LLM {
	t.Helper()
	l := StartLLM()
	t.Cleanup(l.Close)
	return l
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestJSONReply(t *testing.T) {
	l := startLLM(t)
	l.Script("openai.chat",
		Reply{JSON: json.RawMessage(`{"id":"a"}`), Headers: map[string]string{"x-litellm-model-name": "gpt-x"}},
		Reply{Status: 429, JSON: json.RawMessage(`{"error":{"type":"rate_limit_error"}}`)},
	)
	if l.Pending() != 2 {
		t.Fatalf("pending = %d", l.Pending())
	}
	resp := post(t, l.URL+"/v1/chat/completions", `{"model":"m1"}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" ||
		resp.Header.Get("X-Litellm-Model-Name") != "gpt-x" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	if got := readAll(t, resp.Body); got != `{"id":"a"}` {
		t.Fatalf("body = %s", got)
	}
	resp = post(t, l.URL+"/v1/chat/completions", `{"model":"m2"}`)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	got := l.Requests("openai.chat")
	if len(got) != 2 || string(got[0]) != `{"model":"m1"}` || string(got[1]) != `{"model":"m2"}` {
		t.Fatalf("requests = %q", got)
	}
	if l.Pending() != 0 {
		t.Fatalf("pending = %d", l.Pending())
	}
}

func TestRoutes(t *testing.T) {
	l := startLLM(t)
	cases := []struct{ path, route string }{
		{"/v1/chat/completions", "openai.chat"},
		{"/v1/responses", "openai.responses"},
		{"/v1/messages", "anthropic.messages"},
		{"/v1beta/models/gemini-2.5-flash:generateContent", "google.generate"},
		{"/v1beta1/publishers/google/models/gemini-2.5-flash:generateContent", "google.generate"},
		{"/v1/models/gemini-2.5-flash:generateContent?key=k", "google.generate"},
		{"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", "google.stream"},
		{"/model/us.anthropic.claude-sonnet-4%3A0/converse", "bedrock.converse"},
		{"/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fus.x/converse-stream", "bedrock.converse_stream"},
	}
	for _, c := range cases {
		l.Script(c.route, Reply{JSON: json.RawMessage(`{"route":"` + c.route + `"}`)})
		resp := post(t, l.URL+c.path, "{}")
		if got := readAll(t, resp.Body); got != `{"route":"`+c.route+`"}` {
			t.Errorf("%s: %d %s", c.path, resp.StatusCode, got)
		}
	}
}

func TestUnscriptedAndUnknown(t *testing.T) {
	l := startLLM(t)
	resp := post(t, l.URL+"/v1/messages", "{}")
	body := readAll(t, resp.Body)
	if resp.StatusCode != 500 || !strings.Contains(body, "no reply scripted for route anthropic.messages (POST /v1/messages)") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	resp = post(t, l.URL+"/v1/embeddings", "{}")
	body = readAll(t, resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(body, "no route for POST /v1/embeddings") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	get, err := http.Get(l.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	_ = get.Body.Close()
	if get.StatusCode != 404 {
		t.Fatalf("GET: %d", get.StatusCode)
	}
	if len(l.Requests("anthropic.messages")) != 1 {
		t.Fatal("unscripted request not recorded")
	}
}

func TestReset(t *testing.T) {
	l := startLLM(t)
	l.Script("openai.responses", Reply{JSON: json.RawMessage(`{}`)})
	post(t, l.URL+"/v1/messages", "{}")
	l.Reset()
	if l.Pending() != 0 || len(l.Requests("anthropic.messages")) != 0 {
		t.Fatal("reset kept state")
	}
}

func TestSSEReply(t *testing.T) {
	l := startLLM(t)
	l.Script("anthropic.messages", Reply{
		Status:  200,
		Headers: map[string]string{"request-id": "req_1"},
		SSE: []SSEEvent{
			{Event: "message_start", Data: json.RawMessage("{\n  \"type\": \"message_start\"\n}")},
			{Data: json.RawMessage(`"[DONE]"`)},
			{Event: "empty"},
		},
	})
	resp := post(t, l.URL+"/v1/messages", `{"stream":true}`)
	if resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Request-Id") != "req_1" {
		t.Fatalf("headers %v", resp.Header)
	}
	want := "event: message_start\ndata: {\"type\":\"message_start\"}\n\ndata: [DONE]\n\nevent: empty\ndata: null\n\n"
	if got := readAll(t, resp.Body); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestSSEInvalidDataWrittenAsIs(t *testing.T) {
	if got := string(sseFrame(SSEEvent{Data: json.RawMessage("{bad")})); got != "data: {bad\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHang(t *testing.T) {
	l := startLLM(t)
	l.Script("openai.chat", Reply{Hang: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+"/v1/chat/completions", strings.NewReader("{}"))
	_, err := http.DefaultClient.Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestHangAfter(t *testing.T) {
	l := startLLM(t)
	l.Script("openai.chat", Reply{SSE: []SSEEvent{{Data: json.RawMessage(`{"n":1}`)}}, HangAfter: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+"/v1/chat/completions", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	want := "data: {\"n\":1}\n\n"
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != want {
		t.Fatalf("got %q, %v", buf, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("stream ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("read after cancel succeeded")
	}
}

func TestCloseReleasesHangingRequest(t *testing.T) {
	l := StartLLM()
	l.Script("openai.chat", Reply{Hang: true})
	errs := make(chan error, 1)
	go func() {
		resp, err := http.Post(l.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
		if err == nil {
			_ = resp.Body.Close()
		}
		errs <- err
	}()
	for len(l.Requests("openai.chat")) == 0 {
		time.Sleep(time.Millisecond)
	}
	l.Close()
	l.Close()
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the hanging request")
	}
}
