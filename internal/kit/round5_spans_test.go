package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestHeldSpanSettlesWithoutContextCancellation(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, transport := range []bool{false, true} {
			t.Run(fmt.Sprintf("inside=%v/transport=%v", inside, transport), func(t *testing.T) {
				r := reviewRecorder(t)
				ctx := context.Background()
				if inside {
					var owner *rudderstackai.Interaction
					ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
					defer owner.End(rudderstackai.EndOptions{})
				}
				spec := Spec{Provider: "gcp.gemini", Integration: "round5", RetryUnnumbered: true}
				var call *Call
				spec.OnRequest = func(c *Call) { call = c }
				resp, _ := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
					if transport {
						return nil, errors.New("connection refused")
					}
					return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})
				if resp != nil {
					_ = resp.Body.Close()
				}
				defer call.Interaction.End(rudderstackai.EndOptions{})
				deadline := time.Now().Add(3 * time.Second)
				for len(r.named("ai_otel_span")) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				spans := r.named("ai_otel_span")
				if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
					t.Fatalf("failed invocation never settled: %v", spans)
				}
				flush, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if !rudderstackai.Flush(flush) {
					t.Fatal("Flush blocked on settled invocation")
				}
			})
		}
	}
}

func TestUnreadStreamContextCancellationFinishesSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(fmt.Sprint(inside), func(t *testing.T) {
			r := reviewRecorder(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if inside {
				var owner *rudderstackai.Interaction
				ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
				defer owner.End(rudderstackai.EndOptions{})
			}
			var call *Call
			spec := Spec{Provider: "openai", Integration: "round5", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler {
				call = c
				return &chatStream{Observer: NewObserver(c)}
			}}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer call.Interaction.End(rudderstackai.EndOptions{})
			defer func() { _ = resp.Body.Close() }()
			cancel()
			deadline := time.Now().Add(time.Second)
			for len(r.named("ai_otel_span")) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
				t.Fatalf("cancelled stream pending: %v", spans)
			}
			flush, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if !rudderstackai.Flush(flush) {
				t.Fatal("Flush blocked on cancelled stream")
			}
			_ = resp.Body.Close()
			if len(r.named("ai_otel_span")) != 1 {
				t.Fatal("Close finished span again")
			}
		})
	}
}

func TestNullResponseFinishesErrorSpanPreservesProductEvents(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(fmt.Sprint(emit), func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(emit)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			defer owner.End(rudderstackai.EndOptions{})
			spec := Spec{Provider: "openai", Integration: "round5", OnResponse: func(*Call, map[string]any, *http.Response) bool {
				if emit {
					t.Fatal("null reached extraction")
				}
				return false
			}}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`null`))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != "null" {
				t.Fatal("provider response changed")
			}
			_ = resp.Body.Close()
			owner.End(rudderstackai.EndOptions{})
			if len(r.named("ai_llm_response_received")) != 0 || r.named("ai_response_received")[0].Properties["llm_call_count"] != 0 {
				t.Fatal("product behavior changed")
			}
			spans := r.named("ai_otel_span")
			if emit && (len(spans) != 1 || spans[0].Properties["status_code"] != "error") {
				t.Fatalf("null succeeded: %v", spans)
			}
			if !emit && len(spans) != 0 {
				t.Fatal("disabled span emitted")
			}
		})
	}
}

func TestResponseExtractionPanicDropsSpanWithoutChangingResponse(t *testing.T) {
	r := reviewRecorder(t)
	spec := Spec{Provider: "openai", Integration: "round5", OnResponse: func(*Call, map[string]any, *http.Response) bool { panic("extract") }}
	resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "{}" {
		t.Fatal("provider response changed")
	}
	_ = resp.Body.Close()
	spans := r.named("ai_otel_span")
	if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
		t.Fatalf("extraction left span pending: %v", spans)
	}
}

func TestWrapperSetupPanicStillCallsProvider(t *testing.T) {
	reviewRecorder(t)
	old := hook.Tracing.ParentContext
	t.Cleanup(func() { hook.Tracing.ParentContext = old })
	hook.Tracing.ParentContext = func(context.Context) (string, string, string) { panic("parent getter") }
	calls := 0
	spec := Spec{Provider: "gcp.gemini", Integration: "round5"}
	resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err != nil || resp.StatusCode != 200 || calls != 1 {
		t.Fatal("setup broke provider")
	}
	_ = resp.Body.Close()
}

func TestNormalStreamCloseAwaitsCleanupAndReturnsError(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(fmt.Sprint(emit), func(t *testing.T) {
			reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(emit)); err != nil {
				t.Fatal(err)
			}
			cleanup := errors.New("cleanup")
			unblock := make(chan struct{})
			body := &failingStreamBody{readErr: io.EOF, closeErr: cleanup, block: unblock}
			spec := Spec{Provider: "openai", Integration: "round5", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
			resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- resp.Body.Close() }()
			select {
			case <-done:
				t.Fatal("normal Close skipped cleanup wait")
			case <-time.After(20 * time.Millisecond):
			}
			close(unblock)
			select {
			case err := <-done:
				if err != cleanup {
					t.Fatalf("cleanup error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close stuck")
			}
		})
	}
}

func TestUsageFallbackResendCallbackCannotClaimSpan(t *testing.T) {
	r := reviewRecorder(t)
	ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer owner.End(rudderstackai.EndOptions{})
	spec := Spec{Provider: "openai", Integration: "round5", IsStream: func(*http.Request, map[string]any) bool { return true },
		PrepareStream: func(c *Call) *Prepared { return &Prepared{Body: c.Request, Names: []string{"include_usage"}} },
		OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
			return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, ResponseID: "primary", InputTokens: 2})
		}}
	calls := 0
	resp, err := spec.Intercept(reviewRequest(ctx), func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"include_usage unsupported"}}`))}, nil
		}
		if details := rudderstackai.CallDetailsFromContext(req.Context()); details != (rudderstackai.CallDetails{}) {
			t.Fatal("resend details changed")
		}
		owner.RecordCall(rudderstackai.LLMCall{Provider: "openai", Model: "m", ResponseID: "auxiliary", InputTokens: 1})
		if len(r.named("ai_otel_span")) != 0 {
			t.Fatal("resend callback finished primary span")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	owner.End(rudderstackai.EndOptions{})
	spans := r.named("ai_otel_span")
	if calls != 2 || len(spans) != 1 {
		t.Fatalf("requests=%d spans=%d", calls, len(spans))
	}
	for _, call := range r.named("ai_llm_response_received") {
		if call.Properties["response_id"] == "auxiliary" && call.Properties["span_id"] != nil {
			t.Fatal("resend callback stole span")
		}
		if call.Properties["response_id"] == "primary" && call.Properties["span_id"] != spans[0].Properties["span_id"] {
			t.Fatal("primary lost span")
		}
	}
}
