package mcpbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// unwrapOnly hides its writer's Flush behind Unwrap, as some middleware writers do.
type unwrapOnly struct{ http.ResponseWriter }

func (u unwrapOnly) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func TestCanFlush(t *testing.T) {
	for name, tc := range map[string]struct {
		w    http.ResponseWriter
		want bool
	}{
		"flusher":         {httptest.NewRecorder(), true},
		"through unwrap":  {unwrapOnly{unwrapOnly{httptest.NewRecorder()}}, true},
		"no flush":        {struct{ http.ResponseWriter }{httptest.NewRecorder()}, false},
		"unwraps to none": {unwrapOnly{struct{ http.ResponseWriter }{httptest.NewRecorder()}}, false},
	} {
		if got := canFlush(tc.w); got != tc.want {
			t.Errorf("%s: canFlush=%v, want %v", name, got, tc.want)
		}
	}
}

// A ResponseController flush commits the SSE events written so far even when the writer only
// reaches a Flusher through Unwrap.
func TestServeHTTPObservesFlushThroughUnwrap(t *testing.T) {
	responses := NewResponses()
	got := make(chan map[string]any, 1)
	if !responses.Await(1, func(m map[string]any) { got <- m }) {
		t.Fatal("await refused")
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-1,\"message\":\"boom\"}}\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		select {
		case m := <-got:
			if m == nil {
				t.Error("released without the response")
			}
		case <-time.After(5 * time.Second):
			t.Error("flushed response not delivered before the handler returned")
		}
	})
	ServeHTTP(h, unwrapOnly{httptest.NewRecorder()}, httptest.NewRequest(http.MethodPost, "/", nil), responses)
}

// A handler that panics (http.ErrAbortHandler aborts the response) may never send what it wrote:
// waiters are released without it, and the panic carries on.
func TestServeHTTPPanicReleasesWithoutResponse(t *testing.T) {
	for name, contentType := range map[string]string{"json": "application/json", "sse": "text/event-stream"} {
		t.Run(name, func(t *testing.T) {
			responses := NewResponses()
			got := make(chan map[string]any, 1)
			if !responses.Await(1, func(m map[string]any) { got <- m }) {
				t.Fatal("await refused")
			}
			body := `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"boom"}}`
			if contentType == "text/event-stream" {
				body = "event: message\ndata: " + body + "\n\n"
			}
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write([]byte(body))
				panic(http.ErrAbortHandler)
			})
			func() {
				defer func() {
					if p := recover(); p != http.ErrAbortHandler {
						t.Errorf("panic = %v, want http.ErrAbortHandler", p)
					}
				}()
				ServeHTTP(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), responses)
			}()
			if m := <-got; m != nil {
				t.Errorf("aborted response delivered: %v", m)
			}
		})
	}
}

const failure1 = `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"real"}}`

// awaitOne registers a waiter for id 1 and returns what it receives.
func awaitOne(t *testing.T, responses *Responses) <-chan map[string]any {
	t.Helper()
	got := make(chan map[string]any, 1)
	if !responses.Await(1, func(m map[string]any) { got <- m }) {
		t.Fatal("await refused")
	}
	return got
}

func errorText(m map[string]any) any {
	e, _ := m["error"].(map[string]any)
	return e["message"]
}

// A response still buffered when the client goes away was never sent: no text from it.
func TestServeHTTPCancelledRequestDropsUnsentResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		contentType, body string
		flush             bool
		want              any
	}{
		"json buffered": {"application/json", failure1, false, nil},
		"sse unflushed": {"text/event-stream", "data: " + failure1 + "\n\n", false, nil},
		"json flushed":  {"application/json", failure1, true, "real"},
		"sse flushed":   {"text/event-stream", "data: " + failure1 + "\n\n", true, "real"},
	} {
		t.Run(name, func(t *testing.T) {
			responses := NewResponses()
			got := awaitOne(t, responses)
			ctx, cancel := context.WithCancel(context.Background())
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
				if tc.flush {
					_ = http.NewResponseController(w).Flush()
				}
				cancel() // the client goes away
			})
			req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
			ServeHTTP(h, httptest.NewRecorder(), req, responses)
			m := <-got
			if text := errorText(m); m == nil && tc.want != nil || m != nil && text != tc.want {
				t.Errorf("delivered %v, want %v", m, tc.want)
			}
		})
	}
}

// MCP clients read only unnamed and "message" SSE events.
func TestServeHTTPIgnoresNamedSSEEvents(t *testing.T) {
	for name, tc := range map[string]struct {
		stream string
		want   any
	}{
		"named diagnostic first": {"event: diagnostic\ndata: " + strings.Replace(failure1, "real", "internal", 1) + "\n\ndata: " + failure1 + "\n\n", "real"},
		"message event":          {"event: message\ndata: " + failure1 + "\n\n", "real"},
		"only a named event":     {"event: diagnostic\ndata: " + failure1 + "\n\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			responses := NewResponses()
			got := awaitOne(t, responses)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.stream))
			})
			ServeHTTP(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), responses)
			if m := <-got; errorText(m) != tc.want && (m != nil || tc.want != nil) {
				t.Errorf("delivered %v, want %v", m, tc.want)
			}
		})
	}
}

// One message over MaxObserved is not read, however it is split.
func TestServeHTTPBoundsObservedMessages(t *testing.T) {
	big := strings.Repeat("x", MaxObserved/2)
	for name, tc := range map[string]struct{ contentType, body string }{
		"sse data lines": {"text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"a\":\"" + big + "\",\ndata: \"b\":\"" + big + "\",\ndata: \"error\":{\"code\":-1,\"message\":\"real\"}}\n\n"},
		"json body":      {"application/json", `{"jsonrpc":"2.0","id":1,"pad":"` + big + big + `","error":{"code":-1,"message":"real"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			responses := NewResponses()
			got := awaitOne(t, responses)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			})
			ServeHTTP(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), responses)
			if m := <-got; m != nil {
				t.Errorf("read a message over %d bytes: %v", MaxObserved, errorText(m))
			}
		})
	}
}

// An empty write before a request awaits its response decides nothing.
func TestServeHTTPEmptyWriteDecidesNothing(t *testing.T) {
	responses := NewResponses()
	var got <-chan map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(nil)
		got = awaitOne(t, responses)
		_, _ = w.Write([]byte(failure1))
	})
	ServeHTTP(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), responses)
	if m := <-got; errorText(m) != "real" {
		t.Errorf("delivered %v", m)
	}
}

// flushErrOnly can't flush, and says so through FlushError only.
type flushErrOnly struct{ http.ResponseWriter }

func (flushErrOnly) FlushError() error { return http.ErrNotSupported }

// The handler sees http.Flusher only when the writer is one (mcp-go picks SSE by asserting it);
// other flushes still commit, and an unsupported flush keeps the events for the end.
func TestServeHTTPFlushExposure(t *testing.T) {
	for name, tc := range map[string]struct {
		w       http.ResponseWriter
		flusher bool
	}{
		"flusher":        {httptest.NewRecorder(), true},
		"through unwrap": {unwrapOnly{httptest.NewRecorder()}, false},
		"unsupported":    {flushErrOnly{httptest.NewRecorder()}, false},
		"no flush":       {struct{ http.ResponseWriter }{httptest.NewRecorder()}, false},
	} {
		t.Run(name, func(t *testing.T) {
			responses := NewResponses()
			got := awaitOne(t, responses)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, ok := w.(http.Flusher); ok != tc.flusher {
					t.Errorf("handler sees http.Flusher = %v, want %v", ok, tc.flusher)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + failure1 + "\n\n"))
				_ = http.NewResponseController(w).Flush()
			})
			ServeHTTP(h, tc.w, httptest.NewRequest(http.MethodPost, "/", nil), responses)
			if m := <-got; errorText(m) != "real" {
				t.Errorf("delivered %v", m)
			}
		})
	}
}
