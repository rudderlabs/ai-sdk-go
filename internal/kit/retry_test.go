package kit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// attemptPost sends one attempt the way openai-go and anthropic-sdk-go number them.
func (h *harness) attemptPost(ctx context.Context, n int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(chatRequest))
	if err != nil {
		return nil, err
	}
	req.Header.Set(retryCountHeader, strconv.Itoa(n))
	return h.client.Do(req)
}

// failThenServe answers 503 to the first fails requests, then the completion.
func failThenServe(fails int32) http.HandlerFunc {
	var seen atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) <= fails {
			jsonHandler(503, `{"error":{"message":"busy"}}`)(w, r)
			return
		}
		jsonHandler(200, completion)(w, r)
	}
}

func TestRetriedAttemptNotRecorded(t *testing.T) {
	for name, inside := range map[string]bool{"interaction": true, "no interaction": false} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, failThenServe(1))
			ctx, i := context.Background(), (*rudderstackai.Interaction)(nil)
			if inside {
				ctx, i = interaction(h.rec)
			}
			resp, err := h.attemptPost(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close() // unread: the SDK retries
			resp, err = h.attemptPost(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			readAll(t, resp)
			if i != nil {
				i.End(rudderstackai.EndOptions{})
			}
			want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "llm_call_count": 1})
		})
	}
}

func TestFinalAttemptRecordedWhenRead(t *testing.T) {
	h := newHarness(t, failThenServe(1))
	resp, err := h.attemptPost(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp) // the SDK reads the error: the call ends with it
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "llm_call_count": 1})
}

func TestHeldAttemptSettledByNextCallAndEnd(t *testing.T) {
	h := newHarness(t, failThenServe(2))
	ctx, i := interaction(h.rec)
	resp, err := h.attemptPost(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // retry intended, but the app's context ends first
	resp, err = h.attemptPost(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	i.End(rudderstackai.EndOptions{})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "llm_call_count": 2})
}

func TestHeldAttemptOutsideInteractionSettledOnCancel(t *testing.T) {
	h := newHarness(t, failThenServe(1))
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.attemptPost(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cancel()
	// The context watch runs on its own goroutine, then the retry gets its grace period.
	deadline := time.Now().Add(3 * time.Second)
	for !slices.Contains(h.rec.names(), "ai_response_received") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled", "llm_call_count": 1})
}

// Flush waits for a held attempt outside an interaction from the moment it is held, so a Flush
// right after the SDK returns its context error never misses the call the watch settles.
func TestFlushWaitsForHeldAttempt(t *testing.T) {
	h := newHarness(t, failThenServe(1))
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := h.attemptPost(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // the SDK waits to retry
	if rudderstackai.Flush(20 * time.Millisecond) {
		t.Fatal("Flush finished with an attempt still held")
	}
	cancel()
	if !rudderstackai.Flush(5 * time.Second) {
		t.Fatal("Flush timed out")
	}
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled", "llm_call_count": 1})
}

func TestConnectionErrorHeldInsideInteraction(t *testing.T) {
	h := newHarness(t, jsonHandler(200, completion))
	var calls atomic.Int32
	base := h.client.Transport
	h.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return h.spec.Intercept(r, func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
		}
		return base.RoundTrip(r)
	})}
	ctx, i := interaction(h.rec)
	if _, err := h.attemptPost(ctx, 0); err == nil {
		t.Fatal("want the connection error")
	}
	resp, err := h.attemptPost(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	i.End(rudderstackai.EndOptions{})
	want(t, h.rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "llm_call_count": 1})
}
