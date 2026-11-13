package kit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestTerminalStreamWinsOverCancellation(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		for _, closeBody := range []bool{false, true} {
			t.Run(fmt.Sprintf("terminal=%v/close=%v", terminal, closeBody), func(t *testing.T) {
				r := reviewRecorder(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, owner := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice"})
				defer owner.End(rudderstackai.EndOptions{})
				data := "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n"
				if terminal {
					data += "data: [DONE]\n\n"
				}
				spec := Spec{Provider: "openai", Integration: "round7", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
				resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.CopyN(io.Discard, resp.Body, int64(len(data))); err != nil {
					t.Fatal(err)
				}
				if closeBody {
					_ = resp.Body.Close()
				} else {
					cancel()
				}
				limit := time.Now().Add(time.Second)
				// Cancellation records the span before its product call; wait for both
				// before asserting the terminal usage.
				for (len(r.named("ai_otel_span")) == 0 || (terminal && len(r.named("ai_llm_response_received")) == 0)) && time.Now().Before(limit) {
					time.Sleep(time.Millisecond)
				}
				spans := r.named("ai_otel_span")
				want := "error"
				if terminal {
					want = "ok"
				}
				if len(spans) != 1 || spans[0].Properties["status_code"] != want {
					t.Fatalf("terminal=%v spans=%v", terminal, spans)
				}
				if terminal {
					attrs := reviewAttrs(spans[0].Properties)
					if fmt.Sprint(Map(attrs["gen_ai.usage.input_tokens"])["intValue"]) != "3" || fmt.Sprint(Map(attrs["gen_ai.usage.output_tokens"])["intValue"]) != "2" {
						t.Fatalf("terminal span usage lost: %v", attrs)
					}
				}
				calls := r.named("ai_llm_response_received")
				if terminal && (len(calls) != 1 || calls[0].Properties["error_type"] != nil || calls[0].Properties["input_tokens"] != 3) {
					t.Fatalf("terminal usage lost: %v", calls)
				}
				_ = resp.Body.Close()
			})
		}
	}
}

func TestInvocationOutcomeSurvivesOwnerEnd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			r := reviewRecorder(t)
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
			var observer *chatStream
			spec := Spec{Provider: "openai", Integration: "round7", IsStream: func(*http.Request, map[string]any) bool { return stream }, OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
				return c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, InputTokens: 3, OutputTokens: 2})
			}, NewStream: func(c *Call, _ *http.Response) StreamHandler {
				observer = &chatStream{Observer: NewObserver(c)}
				return observer
			}}
			data := `{}`
			header := http.Header{}
			if stream {
				data = "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
				header.Set("Content-Type", "text/event-stream")
			}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				if !stream {
					owner.End(rudderstackai.EndOptions{})
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(data))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if stream {
				owner.End(rudderstackai.EndOptions{})
				if observer == nil {
					t.Fatal("observer missing")
				}
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			rudderstackai.Flush(context.Background())
			spans := r.named("ai_otel_span")
			if len(spans) != 1 || spans[0].Properties["status_code"] != "ok" || spans[0].UserId != "alice" || spans[0].Properties["interaction_id"] != owner.ID() {
				t.Fatalf("settled outcome lost: %v", spans)
			}
		})
	}
}

func TestReaderCloseCancelsBlockedInvocationImmediately(t *testing.T) {
	r := reviewRecorder(t)
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	started := make(chan struct{})
	source := &round6SignallingReader{ReadCloser: reader, started: started}
	spec := Spec{Provider: "openai", Integration: "round7", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
	resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: source}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 1)); done <- err }()
	<-started
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("provider read not aborted")
		}
	case <-time.After(time.Second):
		t.Fatal("Close left provider read blocked")
	}
	spans := r.named("ai_otel_span")
	if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
		t.Fatalf("cancelled span pending: %v", spans)
	}
}
