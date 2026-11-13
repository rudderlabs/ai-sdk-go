package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type failedCallPauseRecorder struct {
	*recorder
	onPause func()
}

func (r *failedCallPauseRecorder) Enqueue(m analytics.Message) error {
	err := r.recorder.Enqueue(m)
	if track, ok := m.(analytics.Track); ok && track.Event == "ai_response_received" && track.Properties["response_status"] == "paused" && r.onPause != nil {
		r.onPause()
	}
	return err
}

func TestFailedCallEventsBeforeEnd(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, failure := range []string{"http_500", "transport_error"} {
			t.Run(fmt.Sprintf("spans=%v/%s", spans, failure), func(t *testing.T) {
				hook.Reset()
				t.Cleanup(func() {
					hook.Reset()
					if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true)); err != nil {
						t.Error(err)
					}
				})
				r := newRecorder(t)
				client := &failedCallPauseRecorder{recorder: r}
				if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithEmitSpans(spans)); err != nil {
					t.Fatal(err)
				}
				ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
				defer owner.End(rudderstackai.EndOptions{})
				spec := Spec{Provider: "openai", Integration: "kit-test"}
				transportErr := errors.New("connection refused")
				send := func(*http.Request) (*http.Response, error) {
					if failure == "transport_error" {
						return nil, transportErr
					}
					return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`))}, nil
				}
				resp, err := spec.Intercept(reviewRequest(ctx), send)
				if failure == "transport_error" {
					if err != transportErr || resp != nil {
						t.Fatalf("transport result changed: response=%v error=%v", resp, err)
					}
				} else {
					if err != nil || resp == nil || resp.StatusCode != http.StatusInternalServerError {
						t.Fatalf("HTTP result changed: response=%v error=%v", resp, err)
					}
					readAll(t, resp)
				}

				// Observe delivery before Pause, End or Flush can drain pending events.
				r.none(t, "ai_response_received")
				if !spans {
					calls := r.named("ai_llm_response_received")
					if len(calls) != 1 {
						t.Errorf("before End/Flush: call events=%d, want 1", len(calls))
					} else {
						want(t, calls[0].Properties, map[string]any{"error_type": "api_error", "call_index": 1})
					}
					r.none(t, "ai_otel_span")
				} else {
					p := r.props(t, "ai_otel_span")
					want(t, p, map[string]any{"status_code": "error"})
					attrs := reviewAttrs(p)
					for _, key := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
						if _, ok := attrs[key]; ok {
							t.Errorf("failed span invented %s", key)
						}
					}
				}

				// A call completing during pause delivery belongs in the persisted
				// pending usage, rather than the segment that Pause just cleared.
				client.onPause = func() {
					resp, _ := spec.Intercept(reviewRequest(ctx), send)
					if resp != nil {
						readAll(t, resp)
					}
				}
				snapshot, ok := owner.Pause(rudderstackai.PauseOptions{})
				client.onPause = nil
				if !ok {
					t.Fatal("pause failed")
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var stored rudderstackai.PausedInteraction
				if err := json.Unmarshal(raw, &stored); err != nil {
					t.Fatal(err)
				}
				if len(stored.PendingCalls) != 1 {
					t.Fatalf("snapshot calls=%d, want 1", len(stored.PendingCalls))
				}
				call := stored.PendingCalls[0]
				if call["usage_unknown"] != spans {
					t.Errorf("snapshot usage_unknown=%v, want %v", call["usage_unknown"], spans)
				}
				if !spans && call["call_event_pending"] != false {
					t.Errorf("snapshot holds already emitted call: %v", call)
				}
				owner.Resume()
				owner.End(rudderstackai.EndOptions{})
				rudderstackai.Flush(context.Background())
				calls := r.named("ai_llm_response_received")
				if len(calls) != 2 {
					t.Fatalf("call events=%d, want 2 without replay", len(calls))
				}
				for index, call := range calls {
					want(t, call.Properties, map[string]any{"error_type": "api_error", "call_index": index + 1})
				}
			})
		}
	}
}
