package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestUnnumberedRetryReusesEphemeralSpanAcrossSpecs(t *testing.T) {
	r := reviewRecorder(t)
	original := reviewRequest(context.Background())
	scope := new(int)
	var calls []*Call
	var child *rudderstackai.Span
	for n := range 2 {
		// Some integrations construct a Spec on each transport entry. RetryScope
		// must preserve ownership even though the Spec itself changes.
		spec := Spec{Provider: "openai", Integration: "lifecycle", RetryUnnumbered: true, RetryScope: scope,
			OnRequest: func(c *Call) { calls = append(calls, c) },
			OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
				return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, Integration: c.Integration, UsageUnknown: true})
			}}
		req := original.Clone(original.Context())
		req.Body, _ = original.GetBody()
		resp, err := spec.Intercept(req, func(req *http.Request) (*http.Response, error) {
			status := http.StatusServiceUnavailable
			if n == 1 {
				status = http.StatusOK
				_, child = rudderstackai.StartSpan(req.Context(), rudderstackai.SpanOptions{Name: "retry child", Operation: "execute_tool"})
				child.End(nil)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if n == 0 && len(r.named("ai_otel_span")) != 0 {
			t.Fatal("retryable failure finished the span before its retry")
		}
	}
	if len(calls) != 2 || calls[0].Span == nil || calls[0].Span != calls[1].Span || calls[0].Interaction != calls[1].Interaction || !calls[0].StartedAt.Equal(calls[1].StartedAt) {
		t.Fatal("retry lost its span, interaction, or start time")
	}
	if !calls[1].Interaction.Ended() {
		t.Fatal("successful retry left the ephemeral interaction open")
	}
	spans := r.named("ai_otel_span")
	if len(spans) != 2 || spans[0].Properties["parent_span_id"] != spans[1].Properties["span_id"] || child.TraceID != spans[1].Properties["trace_id"] {
		t.Fatalf("retry child did not inherit the resumed wrapper: %v", spans)
	}
	if spans[1].Properties["status_code"] != "ok" || len(r.named("ai_llm_response_received")) != 1 {
		t.Fatal("retry emitted a failed attempt or duplicate product call")
	}
}

func TestRetryKeyWithoutBodyFactoryKeepsRequestOwnership(t *testing.T) {
	r := reviewRecorder(t)
	ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	defer owner.End(rudderstackai.EndOptions{})
	spec := &Spec{Provider: "openai", Integration: "lifecycle"}
	first, second := reviewRequest(ctx), reviewRequest(ctx)
	first.GetBody, second.GetBody = nil, nil
	k1, k2 := keyFor(ctx, spec, first, []byte(`{"model":"m"}`)), keyFor(ctx, spec, second, []byte(`{"model":"m"}`))
	if k1 == k2 {
		t.Fatal("identical requests without factories shared retry ownership")
	}
	_, span := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "held", Operation: "chat"})
	a := &attempt{spec: spec, key: k1, call: &Call{Span: span, Interaction: owner, HTTPRequest: first, Provider: "openai", Model: "m"}, errorType: "rate_limit"}
	a.hold()
	if resume(k2, 1) != nil || resume(k1, 0) != nil {
		t.Fatal("another invocation or initial attempt claimed the held span")
	}
	if resume(k1, 1) != a || a.take() {
		t.Fatal("own retry did not claim the attempt exactly once")
	}
	span.End(nil)
	owner.End(rudderstackai.EndOptions{})
	if len(r.named("ai_otel_span")) != 1 || len(r.named("ai_llm_response_received")) != 0 {
		t.Fatal("claimed attempt was settled as a failed product call")
	}
}

func TestRawResponseLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"unmapped object", `{"queued":true}`, 202, false},
		{"null", `null`, 200, false},
		{"no content", "", 204, false},
		{"malformed", `{"broken":`, 200, true},
		{"wrong JSON shape", `[]`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reviewRecorder(t)
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			defer owner.End(rudderstackai.EndOptions{})
			mapped := 0
			spec := Spec{Provider: "openai", Integration: "lifecycle", OnResponse: func(*Call, map[string]any, *http.Response) bool { mapped++; return false }}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tc.body || resp.StatusCode != tc.status {
				t.Fatalf("provider response changed: %q, %v", body, err)
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			owner.End(rudderstackai.EndOptions{})
			wantMapped := 0
			if tc.name == "unmapped object" {
				wantMapped = 1
			}
			if mapped != wantMapped || len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("raw response invented a product call or reached the mapper unexpectedly")
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d, want one after close and interaction end", len(spans))
			}
			status := "ok"
			if tc.wantError {
				status = "error"
			}
			attrs := reviewAttrs(spans[0].Properties)
			if spans[0].Properties["status_code"] != status || !reflect.DeepEqual(attrs["http.response.status_code"], map[string]any{"intValue": fmt.Sprint(tc.status)}) {
				t.Fatalf("raw span status/HTTP status lost: %v", spans[0].Properties)
			}
			for _, k := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
				if _, ok := attrs[k]; ok {
					t.Fatalf("raw response invented %s", k)
				}
			}
		})
	}
}

func TestProviderContextPanicFinishesSpan(t *testing.T) {
	for _, value := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(value.Error(), func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "lifecycle"}
			func() {
				defer func() {
					if got := recover(); got != value {
						t.Fatalf("panic changed: %v", got)
					}
				}()
				_, _ = spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) { panic(value) })
			}()
			spans := r.named("ai_otel_span")
			if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
				t.Fatalf("panic did not finish one failed span: %v", spans)
			}
			want := "timeout"
			if errors.Is(value, context.Canceled) {
				want = "cancelled"
			}
			if p := r.named("ai_llm_response_received"); len(p) != 1 || p[0].Properties["error_type"] != want {
				t.Fatalf("panic category lost: %v", p)
			}
		})
	}
}

func TestStreamUnknownUsageAndCapturedTools(t *testing.T) {
	for _, end := range []End{Completed, Cancelled, Errored} {
		t.Run(fmt.Sprint(end), func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(true)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			defer owner.End(rudderstackai.EndOptions{})
			var observer *Observer
			spec := Spec{Provider: "openai", Integration: "lifecycle", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler {
				observer = NewObserver(c)
				return &chatStream{Observer: observer}
			}}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			observer.NoteTool("tool", "id", "lookup", `{"q":`, "")
			observer.NoteToolArgs("tool", `"value"}`)
			observer.NoteTool("unnamed", "", "", nil, "")
			observer.Record = func(End, string) bool { return false }
			observer.Finish(end, "api_error")
			observer.Finish(end, "api_error")
			_ = resp.Body.Close()
			owner.End(rudderstackai.EndOptions{})
			calls := r.named("ai_llm_response_received")
			if len(calls) != 1 {
				t.Fatalf("calls=%d, want one", len(calls))
			}
			wantTools := []rudderstackai.GenAIToolCall{{Name: "lookup", Arguments: map[string]any{"q": "value"}}}
			if got := observer.toolCalls(); !reflect.DeepEqual(got, wantTools) {
				t.Fatalf("captured tool arguments=%v", got)
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d, want one", len(spans))
			}
			attrs := reviewAttrs(spans[0].Properties)
			for _, k := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
				if _, ok := attrs[k]; ok {
					t.Fatalf("stream without usage invented %s", k)
				}
			}
			if _, ok := attrs["gen_ai.output.messages"]; !ok {
				t.Fatal("stream span lost captured tool calls")
			}
		})
	}
}

func TestConnectionFailureRetryKeepsOneSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, unnumbered := range []bool{false, true} {
			for _, failure := range []error{errors.New("connection reset by peer"), errors.New("connection refused"), context.DeadlineExceeded} {
				t.Run(fmt.Sprintf("inside=%v/unnumbered=%v/%v", inside, unnumbered, failure), func(t *testing.T) {
					r := reviewRecorder(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if inside {
						var i *rudderstackai.Interaction
						ctx, i = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice", Classify: rudderstackai.Ptr(false)})
						defer i.End(rudderstackai.EndOptions{})
					}
					original := reviewRequest(ctx)
					var calls []*Call
					spec := Spec{Provider: "openai", Integration: "review", RetryUnnumbered: unnumbered, OnRequest: func(c *Call) { calls = append(calls, c) }, OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
						return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, Integration: c.Integration, UsageUnknown: true})
					}}
					for n := range 2 {
						req := original.Clone(ctx)
						if n == 0 && failure == context.DeadlineExceeded {
							attemptCtx, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
							defer stop()
							req = req.WithContext(attemptCtx)
						}
						req.Body, _ = original.GetBody()
						req.Header.Set(retryCountHeader, fmt.Sprint(n))
						resp, err := spec.Intercept(req, func(*http.Request) (*http.Response, error) {
							if n == 0 {
								return nil, failure
							}
							return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
						})
						if n == 0 {
							if err != failure {
								t.Fatal("transport error changed")
							}
							if len(r.named("ai_otel_span")) != 0 {
								t.Fatal("failed attempt prematurely emitted span")
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							_ = resp.Body.Close()
						}
					}
					if len(calls) != 2 || calls[0].Span != calls[1].Span || calls[0].Interaction != calls[1].Interaction || !calls[0].StartedAt.Equal(calls[1].StartedAt) {
						t.Fatal("retry lost invocation")
					}
					calls[1].Interaction.End(rudderstackai.EndOptions{})
					spans, events := r.named("ai_otel_span"), r.named("ai_llm_response_received")
					if len(spans) != 1 || len(events) != 1 || spans[0].Properties["span_id"] != events[0].Properties["span_id"] || spans[0].Properties["status_code"] != "ok" {
						t.Fatalf("retry emitted duplicate or failed spans: %v", spans)
					}
				})
			}
		}
	}
}
