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

func TestLateUnreadStreamFinishesOnFlushOrShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			r := reviewRecorder(t)
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
			spec := Spec{Provider: "openai", Integration: "round6", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				owner.End(rudderstackai.EndOptions{})
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if shutdown {
				rudderstackai.Shutdown(context.Background())
			} else {
				rudderstackai.Flush(context.Background())
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
				t.Fatalf("late unread span pending: %v", spans)
			}
			_ = resp.Body.Close()
			rudderstackai.Flush(context.Background())
			if len(r.named("ai_otel_span")) != 1 {
				t.Fatal("finished twice")
			}
		})
	}
}

func TestSpansOffUnmappedEphemeralResponseMatchesMain(t *testing.T) {
	for _, body := range []string{"", `{"queued":true}`, "null", "[]", "bad"} {
		t.Run(body, func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(false)); err != nil {
				t.Fatal(err)
			}
			status := 200
			if body == "" {
				status = 204
			}
			spec := Spec{Provider: "openai", Integration: "round6", OnResponse: func(*Call, map[string]any, *http.Response) bool { return false }}
			resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(got) != body {
				t.Fatal("response changed")
			}
			if len(r.named("ai_response_received")) != 0 || len(r.named("ai_llm_response_received")) != 0 || len(r.named("ai_otel_span")) != 0 {
				t.Fatal("spans-off output differs from main")
			}
		})
	}
}

func TestStreamObserverSetupFailureFinishesSpanPreservesResponse(t *testing.T) {
	for _, panicSetup := range []bool{false, true} {
		t.Run(fmt.Sprint(panicSetup), func(t *testing.T) {
			r := reviewRecorder(t)
			spec := Spec{Provider: "openai", Integration: "round6", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(*Call, *http.Response) StreamHandler {
				if panicSetup {
					panic("observer setup")
				}
				return nil
			}}
			resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "data: {}\n\n" {
				t.Fatal("provider response changed")
			}
			spans := r.named("ai_otel_span")
			if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
				t.Fatalf("observer setup left unfinished span: %v", spans)
			}
		})
	}
}

func TestCancellationFinishesSpanWithBlockedHTTPReader(t *testing.T) {
	r := reviewRecorder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	started := make(chan struct{})
	source := &round6SignallingReader{ReadCloser: reader, started: started}
	defer func() { _ = writer.Close(); _ = reader.Close() }()
	spec := Spec{Provider: "openai", Integration: "round6", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
	resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: source}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = resp.Body.Read(make([]byte, 1)); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Read did not start")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for len(r.named("ai_otel_span")) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if spans := r.named("ai_otel_span"); len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
		t.Fatalf("blocked HTTP reader left pending span: %v", spans)
	}
	_ = writer.Close()
	<-done
	_ = resp.Body.Close()
	if len(r.named("ai_otel_span")) != 1 {
		t.Fatal("finished twice")
	}
}

type round6SignallingReader struct {
	io.ReadCloser
	started chan struct{}
}

func (r *round6SignallingReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	return r.ReadCloser.Read(p)
}
