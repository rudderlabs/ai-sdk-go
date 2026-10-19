// Package testkit holds test helpers shared by the SDK's tests, conformance runner and examples:
// a mock LLM server that serves scripted or recorded provider replies, cassette replay and the
// spec's JSON-schema check.
package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

// Reply is one exact reply on an HTTP JSON or SSE route (spec/fixtures/FIXTURE-FORMAT.md, "replies").
type Reply struct {
	Status    int               `json:"status,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	JSON      json.RawMessage   `json:"json,omitempty"`
	SSE       []SSEEvent        `json:"sse,omitempty"`
	Hang      bool              `json:"hang,omitempty"`
	HangAfter bool              `json:"hang_after,omitempty"`
}

// SSEEvent is one server-sent event. A JSON string Data is written as its raw value (`[DONE]`).
type SSEEvent struct {
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data"`
}

// LLM is a local stand-in for the OpenAI, Anthropic, Google Gen AI and Bedrock Runtime HTTP APIs.
// Every request on a route takes the next queued reply; it never makes up an answer.
type LLM struct {
	// URL is the base URL, without a trailing slash. OpenAI clients use URL + "/v1".
	URL string

	srv      *httptest.Server
	done     chan struct{}
	close    sync.Once
	mu       sync.Mutex
	queues   map[string][]responder
	requests map[string][][]byte
}

// responder serves one queued reply.
type responder interface {
	serve(l *LLM, w http.ResponseWriter, r *http.Request)
}

// StartLLM starts a mock LLM server on a local port.
func StartLLM() *LLM {
	l := &LLM{done: make(chan struct{}), queues: map[string][]responder{}, requests: map[string][][]byte{}}
	l.srv = httptest.NewServer(http.HandlerFunc(l.handle))
	l.URL = l.srv.URL
	return l
}

// Script queues replies for a route (`openai.chat`, `openai.responses`, `anthropic.messages`,
// `google.generate`, `google.stream`), served in order, one per request.
func (l *LLM) Script(route string, replies ...Reply) {
	for _, r := range replies {
		l.enqueue(route, r)
	}
}

func (l *LLM) enqueue(route string, r responder) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queues[route] = append(l.queues[route], r)
}

// Pending returns how many queued replies have not been served yet.
func (l *LLM) Pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, q := range l.queues {
		n += len(q)
	}
	return n
}

// Reset empties the reply queues and the recorded requests.
func (l *LLM) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queues = map[string][]responder{}
	l.requests = map[string][][]byte{}
}

// Requests returns the request bodies received on a route, in order.
func (l *LLM) Requests(route string) [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.requests[route]...)
}

// Close releases hanging requests and stops the server.
func (l *LLM) Close() {
	l.close.Do(func() {
		close(l.done)
		l.srv.Close()
	})
}

var bedrockPath = regexp.MustCompile(`^/model/[^/]+/(converse|converse-stream)$`)

// routeOf maps a request path (escaped, no query) to its route; "" when none.
func routeOf(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return "openai.chat"
	case strings.HasSuffix(path, "/responses"):
		return "openai.responses"
	case strings.HasSuffix(path, "/messages"):
		return "anthropic.messages"
	case strings.HasSuffix(path, ":generateContent"):
		return "google.generate"
	case strings.HasSuffix(path, ":streamGenerateContent"):
		return "google.stream"
	}
	if m := bedrockPath.FindStringSubmatch(path); m != nil {
		if m[1] == "converse" {
			return "bedrock.converse"
		}
		return "bedrock.converse_stream"
	}
	return ""
}

func (l *LLM) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	// The escaped path keeps a `/` inside a Bedrock model ID (an ARN) from splitting the route.
	path := r.URL.EscapedPath()
	route := ""
	if r.Method == http.MethodPost {
		route = routeOf(path)
	}
	if route == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("testkit: no route for %s %s", r.Method, path))
		return
	}
	l.mu.Lock()
	l.requests[route] = append(l.requests[route], body)
	var next responder
	if q := l.queues[route]; len(q) > 0 {
		next, l.queues[route] = q[0], q[1:]
	}
	l.mu.Unlock()
	if next == nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("testkit: no reply scripted for route %s (%s %s)", route, r.Method, path))
		return
	}
	next.serve(l, w, r)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "testkit_error", "message": message}})
	_, _ = w.Write(b)
}

// wait blocks until the client goes away or the server closes.
func (l *LLM) wait(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-l.done:
	}
}

// writeHead sets the content type, `retry-after: 1` on a 429 or 529 (as both reference mocks do),
// then the extra headers, which win.
func writeHead(w http.ResponseWriter, status int, contentType string, headers map[string]string) {
	if status == 0 {
		status = http.StatusOK
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	if status == http.StatusTooManyRequests || status == 529 {
		h.Set("Retry-After", "1")
	}
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
}

func (rep Reply) serve(l *LLM, w http.ResponseWriter, r *http.Request) {
	switch {
	case rep.Hang:
		l.wait(r)
	case rep.SSE != nil:
		writeHead(w, rep.Status, "text/event-stream", rep.Headers)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for _, e := range rep.SSE {
			_, _ = w.Write(sseFrame(e))
			_ = rc.Flush()
		}
		if rep.HangAfter {
			l.wait(r)
		}
	default:
		writeHead(w, rep.Status, "application/json", rep.Headers)
		_, _ = w.Write(rep.JSON)
	}
}

// sseFrame renders one event: data is compacted so a pretty-printed fixture stays on one line.
func sseFrame(e SSEEvent) []byte {
	var b bytes.Buffer
	if e.Event != "" {
		b.WriteString("event: " + e.Event + "\n")
	}
	b.WriteString("data: ")
	var s string
	switch {
	case len(e.Data) == 0:
		b.WriteString("null")
	case json.Unmarshal(e.Data, &s) == nil:
		b.WriteString(s)
	default:
		if json.Compact(&b, e.Data) != nil {
			b.Write(e.Data)
		}
	}
	b.WriteString("\n\n")
	return b.Bytes()
}
