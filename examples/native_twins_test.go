package examples_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
	analytics "github.com/rudderlabs/analytics-go/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
	"github.com/rudderlabs/ai-sdk-go/rudderotel"
)

type recorder struct {
	mu     sync.Mutex
	events []analytics.Track
}

func (r *recorder) Enqueue(m analytics.Message) error {
	if e, ok := m.(analytics.Track); ok {
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
	}
	return nil
}
func (*recorder) Close() error                { return nil }
func (*recorder) Flush(context.Context) error { return nil }
func (r *recorder) all() []analytics.Track {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]analytics.Track(nil), r.events...)
}

type localTransport struct {
	base http.RoundTripper
	host string
}

func (l localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != l.host {
		return nil, fmt.Errorf("non-localhost request rejected: %s", r.URL.Host)
	}
	return l.base.RoundTrip(r)
}

const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_local\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
const finish = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestNativeAnthropicFailedTwinsUnbilled(t *testing.T) {
	for _, emitSpans := range []bool{false, true} {
		for _, scenario := range []string{"success", "http500", "timeout", "cancelled", "stream_success", "stream_error", "stream_truncated", "stream_cancelled", "stream_timeout", "stream_close"} {
			t.Run(fmt.Sprintf("sdk_spans_%t/%s", emitSpans, scenario), func(t *testing.T) {
				hook.Reset()
				t.Cleanup(hook.Reset)
				r := &recorder{}
				var hooks atomic.Int64
				if err := ai.Configure(ai.WithAnalytics(r), ai.WithLLMCallEvents(true), ai.WithEmitSpans(emitSpans), ai.WithCredits(func(c ai.CreditCall) (*float64, error) {
					hooks.Add(1)
					return ai.Ptr(float64(c.InputTokens + c.OutputTokens)), nil
				})); err != nil {
					t.Fatal(err)
				}
				p, err := rudderotel.NewSpanProcessor(rudderotel.Options{Analytics: r, ProductEvents: true, LinkTraceID: true, Simple: true})
				if err != nil {
					t.Fatal(err)
				}
				capture := tracetest.NewSpanRecorder()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(capture), sdktrace.WithSpanProcessor(p))
				defer func() {
					if err := tp.Shutdown(context.Background()); err != nil {
						t.Error(err)
					}
				}()
				arrived := make(chan struct{})
				stopServer := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					close(arrived)
					switch scenario {
					case "http500":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(500)
						fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"local failure"}}`)
					case "timeout", "cancelled":
						select {
						case <-req.Context().Done():
						case <-stopServer:
						}
					default:
						if strings.HasPrefix(scenario, "stream_") {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, start)
							w.(http.Flusher).Flush()
							switch scenario {
							case "stream_error":
								fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"local stream failure\"}}\n\n")
							case "stream_truncated": // genuine EOF after partial usage, without message_stop
							case "stream_success":
								fmt.Fprint(w, finish)
							default:
								select {
								case <-req.Context().Done():
								case <-stopServer:
								}
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"id":"msg_local","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`)
						}
					}
				}))
				defer server.Close()
				defer close(stopServer)
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				client := anthropic.NewClient(aoption.WithAPIKey("local-test"), aoption.WithBaseURL(server.URL), aoption.WithMaxRetries(0), aoption.WithHTTPClient(&http.Client{Transport: localTransport{transport, strings.TrimPrefix(server.URL, "http://")}}), aoption.WithTracerProvider(tp), aoption.WithOpenTelemetryTraces(true), rudderanthropic.WithTracking())
				ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "local-test", Classify: ai.Ptr(false)})
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if scenario == "timeout" || scenario == "stream_timeout" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 15*time.Millisecond)
					defer stop()
				}
				if scenario == "cancelled" {
					go func() { <-arrived; cancel() }()
				}
				params := anthropic.MessageNewParams{Model: "claude-sonnet-4-6", MaxTokens: 16, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))}}
				var callErr error
				if strings.HasPrefix(scenario, "stream_") {
					stream := client.Messages.NewStreaming(ctx, params)
					done := make(chan struct{})
					// The context is explicitly propagated to a different consumer goroutine.
					go func() {
						defer close(done)
						first := true
						for stream.Next() {
							if first {
								first = false
								if scenario == "stream_cancelled" {
									cancel()
								}
								if scenario == "stream_close" {
									break
								}
							}
						}
						callErr = stream.Err()
						if e := stream.Close(); callErr == nil {
							callErr = e
						}
					}()
					<-done
				} else {
					_, callErr = client.Messages.New(ctx, params)
				}
				success := scenario == "success" || scenario == "stream_success"
				if success && callErr != nil {
					t.Fatalf("successful control failed: %v", callErr)
				}
				if !success && scenario != "stream_close" && scenario != "stream_truncated" && callErr == nil {
					t.Fatal("provider did not fail")
				}
				// Flush waits for asynchronous wrapper cancellation completion, without ending pricing early.
				if !ai.Flush(context.Background()) {
					t.Fatal("flush failed")
				}
				i.End(ai.EndOptions{})
				spans := capture.Ended()
				if len(spans) != 1 {
					t.Fatalf("expected one native span, got %d", len(spans))
				}
				calls, responses := 0, 0
				billed := 0
				nativeIndex, wrapperIndex := -1, -1
				exportedSpans := 0
				for index, e := range r.all() {
					switch e.Event {
					case "ai_otel_span":
						exportedSpans++
						if e.Properties["span_name"] == "anthropic.messages.create" {
							nativeIndex = index
						} else {
							wrapperIndex = index
						}
					case "ai_llm_response_received":
						calls++
						if _, ok := e.Properties["credits"]; ok {
							billed++
						}
						if !success && e.Properties["error_type"] == nil {
							t.Errorf("failed call missing error: %v", e.Properties)
						}
					case "ai_response_received":
						responses++
						if _, ok := e.Properties["total_credits"]; ok {
							billed++
						}
						if e.Properties["llm_call_count"] != 1 {
							t.Errorf("expected one merged call: %v", e.Properties)
						}
						if !success && e.Properties["response_status"] == "success" {
							t.Errorf("failed response reported success: %v", e.Properties)
						}
					}
				}
				if calls != 1 || responses != 1 {
					t.Fatalf("calls=%d responses=%d events=%v", calls, responses, r.all())
				}
				// Main emits only the SDK span; the other processor still receives native
				// instrumentation unchanged. The plain-setup regressions exercise both report orders.
				if exportedSpans != 1 {
					t.Fatalf("expected one span per call, got %d", exportedSpans)
				}
				if emitSpans {
					if nativeIndex >= 0 || wrapperIndex < 0 {
						t.Fatal("SDK span did not cover native twin")
					}

				} else if nativeIndex < 0 || wrapperIndex >= 0 {
					t.Fatal("spans-off native export changed")
				}
				t.Logf("RESULT provider=anthropic sdk_spans=%t scenario=%s failed=%t call_billed=%t response_billed=%t hooks=%d native_status=%s native_index=%d wrapper_index=%d", emitSpans, scenario, !success, billed > 0, billed > 1, hooks.Load(), spans[0].Status().Code, nativeIndex, wrapperIndex)
				if !success && billed > 0 {
					t.Fatalf("FAILED CALL BILLED: hook_invocations=%d events=%v", hooks.Load(), r.all())
				}
				if success && (billed != 2 || hooks.Load() == 0) {
					t.Fatalf("billing control inactive: billed=%d hooks=%d", billed, hooks.Load())
				}
			})
		}
	}
}
