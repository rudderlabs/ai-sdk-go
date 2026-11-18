package examples_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	ooption "github.com/openai/openai-go/v3/option"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
	"github.com/rudderlabs/ai-sdk-go/rudderotel"
)

const chatStart = `data: {"id":"chat_local","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}` + "\n\n"
const chatFinish = `data: {"id":"chat_local","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\ndata: [DONE]\n\n"

func TestOpenAIFailedTwinsUnbilled(t *testing.T) {
	for _, emitSpans := range []bool{false, true} {
		for _, scenario := range []string{"success", "http500", "timeout", "cancelled", "stream_success", "stream_error", "stream_cancelled", "stream_timeout", "stream_close"} {
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
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p))
				defer func() {
					if e := tp.Shutdown(context.Background()); e != nil {
						t.Error(e)
					}
				}()
				arrived, stopServer := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					close(arrived)
					switch scenario {
					case "http500":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(500)
						fmt.Fprint(w, `{"error":{"code":"api_error","message":"local failure"}}`)
					case "timeout", "cancelled":
						select {
						case <-req.Context().Done():
						case <-stopServer:
						}
					default:
						if strings.HasPrefix(scenario, "stream_") {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, chatStart)
							w.(http.Flusher).Flush()
							switch scenario {
							case "stream_success":
								fmt.Fprint(w, chatFinish)
							case "stream_error":
								fmt.Fprint(w, "data: {\"error\":{\"code\":\"api_error\",\"message\":\"local stream failure\"}}\n\n")
							default:
								select {
								case <-req.Context().Done():
								case <-stopServer:
								}
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"id":"chat_local","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
						}
					}
				}))
				defer server.Close()
				defer close(stopServer)
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				client := openai.NewClient(ooption.WithAPIKey("local-test"), ooption.WithBaseURL(server.URL), ooption.WithMaxRetries(0), ooption.WithHTTPClient(&http.Client{Transport: localTransport{transport, strings.TrimPrefix(server.URL, "http://")}}), ooption.WithUnsafeAllowHTTP(), rudderopenai.WithTracking())
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
				// This is the documented application instrumentation around the actual provider call.
				// Outcomes, response IDs and usage below come only from the real SDK result.
				ctx, span := tp.Tracer("local-app").Start(ctx, "chat gpt-4o", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "gpt-4o")))
				params := openai.ChatCompletionNewParams{Model: "gpt-4o", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")}}
				var callErr error
				var input, output int64
				var responseID string
				if strings.HasPrefix(scenario, "stream_") {
					stream := client.Chat.Completions.NewStreaming(ctx, params)
					done := make(chan struct{})
					go func() {
						defer close(done)
						first := true
						for stream.Next() {
							chunk := stream.Current()
							responseID = chunk.ID
							if chunk.Usage.PromptTokens > 0 {
								input = chunk.Usage.PromptTokens
								output = chunk.Usage.CompletionTokens
							}
							if first {
								first = false
								if scenario == "stream_cancelled" {
									cancel()
								}
								if scenario == "stream_close" {
									callErr = context.Canceled
									break
								}
							}
						}
						if e := stream.Err(); e != nil {
							callErr = e
						}
						if e := stream.Close(); callErr == nil {
							callErr = e
						}
						if ctx.Err() != nil {
							callErr = ctx.Err()
						}
						finishAppSpan(span, callErr, responseID, input, output)
					}()
					<-done
				} else {
					result, e := client.Chat.Completions.New(ctx, params)
					callErr = e
					if result != nil {
						responseID = result.ID
						input = result.Usage.PromptTokens
						output = result.Usage.CompletionTokens
					}
					finishAppSpan(span, callErr, responseID, input, output)
				}
				success := scenario == "success" || scenario == "stream_success"
				if success && callErr != nil {
					t.Fatalf("success control failed: %v", callErr)
				}
				if !success && callErr == nil {
					t.Fatal("provider did not fail")
				}
				if !ai.Flush(context.Background()) {
					t.Fatal("flush failed")
				}
				i.End(ai.EndOptions{})
				calls, responses, billed := 0, 0, 0
				for _, e := range r.all() {
					switch e.Event {
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
							t.Errorf("expected one twin: %v", e.Properties)
						}
						if !success && e.Properties["response_status"] == "success" {
							t.Errorf("failed response reported success: %v", e.Properties)
						}
					}
				}
				t.Logf("RESULT provider=openai sdk_spans=%t scenario=%s failed=%t call_billed=%t response_billed=%t hooks=%d", emitSpans, scenario, !success, billed > 0, billed > 1, hooks.Load())
				if calls != 1 || responses != 1 {
					t.Fatalf("calls=%d responses=%d", calls, responses)
				}
				if !success && billed > 0 {
					t.Fatalf("FAILED CALL BILLED: events=%v", r.all())
				}
				if success && (billed != 2 || hooks.Load() == 0) {
					t.Fatalf("billing control inactive: billed=%d hooks=%d", billed, hooks.Load())
				}
			})
		}
	}
}

func finishAppSpan(span trace.Span, err error, responseID string, input, output int64) {
	if responseID != "" {
		span.SetAttributes(attribute.String("gen_ai.response.id", responseID))
	}
	if input > 0 {
		span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", input), attribute.Int64("gen_ai.usage.output_tokens", output))
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String("error.type", ai.ErrorTypeOf(err)))
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}
