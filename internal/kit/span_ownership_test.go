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
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

type failingStreamBody struct {
	readErr, closeErr error
	closed            bool
	block             <-chan struct{}
}

func (b *failingStreamBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b *failingStreamBody) Close() error {
	b.closed = true
	if b.block != nil {
		<-b.block
	}
	return b.closeErr
}

func TestStreamProviderErrorDoesNotRunCleanup(t *testing.T) {
	for _, emit := range []bool{false, true} {
		for _, blocking := range []bool{false, true} {
			t.Run(fmt.Sprintf("emit=%v/blocking=%v", emit, blocking), func(t *testing.T) {
				r := reviewRecorder(t)
				if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(emit)); err != nil {
					t.Fatal(err)
				}
				sentinel := errors.New("provider failure")
				unblock := make(chan struct{})
				defer close(unblock)
				body := &failingStreamBody{readErr: sentinel, closeErr: errors.New("cleanup failure")}
				if blocking {
					body.block = unblock
				}
				spec := Spec{Provider: "openai", Integration: "review", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler { return &chatStream{Observer: NewObserver(c)} }}
				resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { _, err := io.ReadAll(resp.Body); done <- err }()
				select {
				case err := <-done:
					if err != sentinel || body.closed {
						t.Fatalf("err=%v closed=%v", err, body.closed)
					}
				case <-time.After(time.Second):
					t.Fatal("provider error delayed by cleanup")
				}
				want := 0
				if emit {
					want = 1
				}
				if got := len(r.named("ai_otel_span")); got != want {
					t.Fatalf("spans=%d want %d", got, want)
				}
			})
		}
	}
}

func TestInvalidJSONFinishesWrapperSpanWithErrorOnce(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(fmt.Sprint(emit), func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(emit)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			spec := Spec{Provider: "openai", Integration: "review", OnResponse: func(*Call, map[string]any, *http.Response) bool {
				t.Fatal("invalid JSON reached product mapper")
				return false
			}}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"broken":`))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err == nil {
				t.Fatal("provider parse error lost")
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			owner.End(rudderstackai.EndOptions{})
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("parse failure invented product call")
			}
			if p := r.named("ai_response_received")[0].Properties; p["llm_call_count"] != 0 {
				t.Fatalf("product call count changed: %v", p)
			}
			spans := r.named("ai_otel_span")
			if !emit {
				if len(spans) != 0 {
					t.Fatal("disabled span emitted")
				}
				return
			}
			if len(spans) != 1 || spans[0].Properties["status_code"] != "error" {
				t.Fatalf("parse failure spans=%v", spans)
			}
		})
	}
}

func TestInvalidJSONPreservesEphemeralProductBehavior(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(fmt.Sprint(emit), func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(emit)); err != nil {
				t.Fatal(err)
			}
			var owner *rudderstackai.Interaction
			spec := Spec{Provider: "openai", Integration: "review", OnRequest: func(c *Call) { owner = c.Interaction }}
			resp, err := spec.Intercept(reviewRequest(context.Background()), func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("invalid JSON"))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.End(rudderstackai.EndOptions{})
			_ = resp.Body.Close()
			// origin/main leaves this interaction open and records no product call.
			if owner.Ended() || len(r.named("ai_response_received")) != 0 || len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("invalid JSON changed ephemeral product events")
			}
		})
	}
}
