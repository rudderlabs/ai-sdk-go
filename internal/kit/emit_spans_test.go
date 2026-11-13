package kit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestWrapperTransportErrorCategory(t *testing.T) {
	hook.Reset()
	t.Cleanup(func() {
		hook.Reset()
		if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true)); err != nil {
			t.Error(err)
		}
	})
	r := newRecorder(t)
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r), rudderstackai.WithEmitSpans(true), rudderstackai.WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", strings.NewReader(`{"model":"m"}`))
	spec := Spec{Provider: "openai", Integration: "kit-test"}
	want := errors.New("failed a@example.com")
	_, err := spec.Intercept(req, func(*http.Request) (*http.Response, error) { return nil, want })
	if err != want {
		t.Fatal("transport error changed")
	}
	i.End(rudderstackai.EndOptions{})
	spans := r.named("ai_otel_span")
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	p := spans[0].Properties
	if p["status_message"] != "api_error" {
		t.Fatalf("status_message=%v", p["status_message"])
	}
	rs := p["otlp"].(map[string]any)["resourceSpans"].([]map[string]any)[0]
	ss := rs["scopeSpans"].([]map[string]any)[0]
	span := ss["spans"].([]map[string]any)[0]
	events := span["events"].([]map[string]any)
	found := false
	for _, kv := range events[0]["attributes"].([]map[string]any) {
		if kv["key"] == "exception.type" {
			found = kv["value"].(map[string]any)["stringValue"] == "string"
		}
	}
	if !found {
		t.Fatal("normalized error category type lost")
	}
}

func TestWrapperRawAndUnreadSpan(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			hook.Reset()
			t.Cleanup(func() {
				hook.Reset()
				if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true)); err != nil {
					t.Error(err)
				}
			})
			r := newRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r), rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true)); err != nil {
				t.Fatal(err)
			}
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", strings.NewReader(`{"model":"m"}`))
			spec := Spec{Provider: "openai", Integration: "kit-test", IsStream: func(*http.Request, map[string]any) bool { return stream }, OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
				return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, Integration: c.Integration, InputTokens: 1, OutputTokens: 1})
			}, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
			var child *rudderstackai.Span
			resp, err := spec.Intercept(req, func(req *http.Request) (*http.Response, error) {
				_, child = rudderstackai.StartSpan(req.Context(), rudderstackai.SpanOptions{Name: "tool", Operation: "execute_tool"})
				child.End(nil)
				header := http.Header{}
				body := `{}`
				if stream {
					header.Set("Content-Type", "text/event-stream")
					body = "data: [DONE]\n\n"
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !stream {
				if _, err := io.ReadAll(resp.Body); err != nil {
					t.Fatal(err)
				}
			}
			i.End(rudderstackai.EndOptions{})
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 2 {
				t.Fatalf("spans=%d", len(spans))
			}
			if spans[0].Properties["parent_span_id"] != spans[1].Properties["span_id"] || child.TraceID != spans[1].Properties["trace_id"] {
				t.Fatal("tool did not inherit active wrapper")
			}
		})
	}
}
