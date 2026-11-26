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

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func reviewRecorder(t *testing.T) *recorder {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	r := newRecorder(t)
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r), rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true)); err != nil {
		t.Fatal(err)
	}
	return r
}
func reviewAttrs(p map[string]any) map[string]any {
	rs := p["otlp"].(map[string]any)["resourceSpans"].([]map[string]any)[0]
	s := rs["scopeSpans"].([]map[string]any)[0]["spans"].([]map[string]any)[0]
	out := map[string]any{}
	for _, kv := range s["attributes"].([]map[string]any) {
		out[kv["key"].(string)] = kv["value"]
	}
	return out
}
func reviewRequest(ctx context.Context) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", strings.NewReader(`{"model":"m"}`))
	return req
}
func TestProviderFailureUsageUnknown(t *testing.T) {
	for _, status := range []int{0, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "review"}
			_, _ = spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				if status == 0 {
					return nil, errors.New("failure")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			got := reviewAttrs(r.named("ai_otel_span")[0].Properties)
			for _, k := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
				if _, ok := got[k]; ok {
					t.Errorf("invented %s", k)
				}
			}
		})
	}
}
func TestRawResponseOnlyFinishesSpan(t *testing.T) {
	for _, raw := range []string{"", "not JSON"} {
		t.Run(raw, func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "review"}
			var i *rudderstackai.Interaction
			spec.OnRequest = func(c *Call) { i = c.Interaction }
			resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if !i.Ended() {
				t.Error("ephemeral interaction still open")
			}
			if responses := r.named("ai_response_received"); len(responses) == 1 && responses[0].Properties["llm_call_count"] != 0 {
				t.Error("raw response increased call count")
			}
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Error("invented product call")
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d", len(spans))
			}
			if !reflect.DeepEqual(reviewAttrs(spans[0].Properties)["gen_ai.response.model"], map[string]any{"stringValue": "m"}) {
				t.Error("raw response model missing")
			}
			if !reflect.DeepEqual(reviewAttrs(spans[0].Properties)["http.response.status_code"], map[string]any{"intValue": "204"}) {
				t.Error("raw status missing")
			}
		})
	}
}
func TestProviderPanicFinishesSpanAndRepanics(t *testing.T) {
	for _, value := range []any{errors.New("sentinel"), "panic text", &struct{ N int }{7}} {
		t.Run(fmt.Sprintf("%T", value), func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "review"}
			var got any
			func() {
				defer func() { got = recover() }()
				_, _ = spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) { panic(value) })
			}()
			if got != value {
				t.Fatal("original panic changed")
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d", len(spans))
			}
			if spans[0].Properties["status_code"] != "error" {
				t.Fatal("panic span not failed")
			}
		})
	}
}
func TestWrapperExactNormalAttributes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "review", OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
				return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, Integration: c.Integration, InputTokens: 2, OutputTokens: 1})
			}}
			_, _ = spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				if failed {
					return nil, errors.New("fail")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d", len(spans))
			}
			p := spans[0].Properties
			want := map[string]any{"gen_ai.operation.name": map[string]any{"stringValue": "chat"}, "gen_ai.provider.name": map[string]any{"stringValue": "openai"}, "gen_ai.request.model": map[string]any{"stringValue": "m"}, "gen_ai.response.model": map[string]any{"stringValue": "m"}, "rudderstack.span_source": map[string]any{"stringValue": "wrapper"}, "rudderstack.interaction_id": map[string]any{"stringValue": p["interaction_id"]}, "gen_ai.conversation.id": map[string]any{"stringValue": p["conversation_id"]}}
			// The default anonymous identity is included in the span.
			if id, ok := p["anonymous_id"]; ok {
				want["rudderstack.anonymous_id"] = map[string]any{"stringValue": id}
			}
			if !failed {
				want["gen_ai.usage.input_tokens"] = map[string]any{"intValue": "2"}
				want["gen_ai.usage.output_tokens"] = map[string]any{"intValue": "1"}
			}
			if got := reviewAttrs(p); !reflect.DeepEqual(got, want) {
				t.Fatalf("attrs=%v want=%v", got, want)
			}
		})
	}
}
func TestReusedTransportErrorEmitsEachInvocation(t *testing.T) {
	r := reviewRecorder(t)
	spec := Spec{Provider: "openai", Integration: "review"}
	err := errors.New("same error")
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	for range 2 {
		_, _ = spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) { return nil, err })
	}
	i.End(rudderstackai.EndOptions{})
	if len(r.named("ai_otel_span")) != 2 {
		t.Fatal("reused error suppressed an invocation")
	}
}

func TestIdenticalRetryInvocationsKeepOwnership(t *testing.T) {
	r := reviewRecorder(t)
	spec := Spec{Provider: "openai", Integration: "review", OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
		return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, Integration: c.Integration, UsageUnknown: true})
	}}
	originals := []*http.Request{reviewRequest(context.Background()), reviewRequest(context.Background())}
	calls := []*Call{}
	spec.OnRequest = func(c *Call) { calls = append(calls, c) }
	for n := 0; n < 2; n++ {
		for _, original := range originals {
			req := original.Clone(original.Context())
			req.Body, _ = original.GetBody()
			req.Header.Set(retryCountHeader, fmt.Sprint(n))
			resp, err := spec.Intercept(req, func(*http.Request) (*http.Response, error) {
				status := 200
				if n == 0 {
					status = 503
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	if calls[0].Span != calls[2].Span || calls[1].Span != calls[3].Span || calls[0].Interaction != calls[2].Interaction || calls[1].Interaction != calls[3].Interaction {
		t.Error("retry claimed another invocation")
	}
	if got := len(r.named("ai_otel_span")); got != 2 {
		t.Fatalf("spans=%d want 2", got)
	}
}
