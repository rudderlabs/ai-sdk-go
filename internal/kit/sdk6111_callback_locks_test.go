package kit

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

type reentryHandler struct {
	event  func()
	finish func()
}

func (h *reentryHandler) OnEvent(string, []byte) {
	if h.event != nil {
		h.event()
	}
}
func (h *reentryHandler) Finish(End, string) {
	if h.finish != nil {
		h.finish()
	}
}
func (h *reentryHandler) Complete() bool { return false }
func TestSDK6111StreamCallbacksReenter(t *testing.T) {
	for _, kind := range []string{"event", "finish"} {
		for _, threaded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				h := &reentryHandler{}
				body := io.NopCloser(strings.NewReader("data: value\n\n"))
				b := &sseBody{src: body, reader: bufio.NewReader(body), h: h, ctx: context.Background()}
				progress := make(chan bool, 1)
				fn := func() {
					if threaded {
						done := make(chan struct{})
						go func() { defer close(done); _ = b.Close() }()
						select {
						case <-done:
							progress <- true
						case <-time.After(time.Second):
							progress <- false
						}
					} else {
						_ = b.Close()
						progress <- true
					}
				}
				if kind == "event" {
					h.event = fn
				} else {
					h.finish = fn
				}
				done := make(chan struct{})
				go func() { defer close(done); _, _ = io.ReadAll(b) }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("stream callback deadlocked")
				}
				select {
				case ok := <-progress:
					if !ok {
						t.Fatal("callback blocked")
					}
				case <-time.After(time.Second):
					t.Fatal("callback not invoked")
				}
			})
		}
	}
}

type retryCallbackClient struct {
	*recorder
	callback func()
}

func (c *retryCallbackClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if c.callback != nil {
		c.callback()
	}
	return err
}

func TestSDK6111RetryBodyCallbackReenter(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			r := newRecorder(t)
			client := &retryCallbackClient{recorder: r}
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithEmitSpans(false)); err != nil {
				t.Fatal(err)
			}
			_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Analytics: client})
			req, err := http.NewRequest(http.MethodPost, "http://provider.example", nil)
			if err != nil {
				t.Fatal(err)
			}
			a := &attempt{spec: &Spec{}, call: &Call{Interaction: i, HTTPRequest: req, Provider: "openai", Model: "gpt-4o", StartedAt: time.Now()}, errorType: "api_error"}
			retried.mu.Lock()
			retried.m[a.key] = a
			retried.mu.Unlock()
			b := &probeBody{ReadCloser: io.NopCloser(strings.NewReader("body")), a: a}
			progress := make(chan bool, 1)
			var fired atomic.Bool
			client.callback = func() {
				if !fired.CompareAndSwap(false, true) {
					return
				}
				if threaded {
					done := make(chan struct{})
					go func() { defer close(done); _ = b.Close() }()
					select {
					case <-done:
						progress <- true
					case <-time.After(time.Second):
						progress <- false
					}
				} else {
					_ = b.Close()
					progress <- true
				}
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = io.ReadAll(b) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("retry callback deadlocked")
			}
			select {
			case ok := <-progress:
				if !ok {
					t.Fatal("retry callback blocked")
				}
			case <-time.After(time.Second):
				t.Fatal("callback not invoked")
			}
			i.End(rudderstackai.EndOptions{})
		})
	}
}
