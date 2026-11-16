package mcpbridge

import (
	"net/http"
	"net/http/httptest"
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
