package ruddergenai_test

import (
	"context"
	"fmt"
	"net/http"
	"syscall"
	"testing"
	"time"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

type refusingSpanTransport struct{ attempts int }

func (r *refusingSpanTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.attempts++
	return nil, syscall.ECONNREFUSED
}

func TestGoogleConnectionRefusalFinishesSpanAndFlush(t *testing.T) {
	for _, attempts := range []int32{0, 1, 2} {
		for _, inside := range []bool{false, true} {
			t.Run(fmt.Sprintf("attempts=%d/inside=%v", attempts, inside), func(t *testing.T) {
				rec := setup(t, rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true))
				rt := &refusingSpanTransport{}
				client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(&http.Client{Transport: rt}))
				ctx := context.Background()
				var owner *rudderstackai.Interaction
				if inside {
					ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
					defer owner.End(rudderstackai.EndOptions{})
				}
				var config *genai.GenerateContentConfig
				if attempts > 0 {
					config = &genai.GenerateContentConfig{HTTPOptions: &genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{Attempts: &attempts, InitialDelay: rudderstackai.Ptr(0.001)}}}
				}
				_, err := client.Models.GenerateContent(ctx, model, genai.Text("hello"), config)
				if err == nil {
					t.Fatal("provider error lost")
				}
				flush, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				if !rudderstackai.Flush(flush) {
					t.Fatal("Flush timed out after settled Google invocation")
				}
				// Explicit interactions are not tracked by Flush; allow the bounded retry grace.
				deadline := time.Now().Add(2 * time.Second)
				for {
					rec.mu.Lock()
					spans := 0
					for _, m := range rec.msgs {
						if m.Event == "ai_otel_span" {
							spans++
						}
					}
					rec.mu.Unlock()
					if spans > 0 || time.Now().After(deadline) {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				spans := 0
				for _, m := range rec.events(t) {
					if m.Event == "ai_otel_span" {
						spans++
						if m.Properties["status_code"] != "error" {
							t.Fatal("refusal succeeded")
						}
					}
				}
				if spans != 1 {
					t.Fatalf("spans=%d", spans)
				}
				want := int(attempts)
				if want == 0 {
					want = 1
				}
				if rt.attempts != want {
					t.Fatalf("attempts=%d want %d", rt.attempts, want)
				}
			})
		}
	}
}
