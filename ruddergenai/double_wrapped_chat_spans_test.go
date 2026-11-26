package ruddergenai_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

type indirectTrackedTransport struct{ wrapped http.RoundTripper }

func (t indirectTrackedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.wrapped.RoundTrip(r)
}

func TestGoogleChatDoubleWrappedOneSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, async := range []bool{false, true} {
				t.Run(fmt.Sprintf("inside=%v/stream=%v/goroutine=%v", inside, stream, async), func(t *testing.T) {
					rec := setup(t, rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true))
					route := "google.generate"
					if stream {
						route = "google.stream"
						llm.Script(route, sseReply(reply([]any{m{"text": "Hi"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 1}, "STOP")))
					} else {
						llm.Script(route, jsonReply(reply([]any{m{"text": "Hi"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 1}, "STOP")))
					}
					hc := &http.Client{Transport: ruddergenai.Transport(indirectTrackedTransport{wrapped: ruddergenai.Transport(nil)})}
					client := newClient(t, genai.BackendGeminiAPI, hc)
					ctx := context.Background()
					var owner *rudderstackai.Interaction
					if inside {
						ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice"})
						defer owner.End(rudderstackai.EndOptions{})
					}
					chat, err := client.Chats.Create(ctx, model, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					invoke := func() error {
						if stream {
							for _, err := range chat.SendMessageStream(ctx, genai.Part{Text: "Hi"}) {
								if err != nil {
									return err
								}
							}
							return nil
						}
						_, err := chat.SendMessage(ctx, genai.Part{Text: "Hi"})
						return err
					}
					if async {
						done := make(chan error, 1)
						go func() { done <- invoke() }()
						err = <-done
					} else {
						err = invoke()
					}
					if err != nil {
						t.Fatal(err)
					}
					if owner != nil {
						owner.End(rudderstackai.EndOptions{})
					}
					spans, calls := 0, 0
					for _, m := range rec.events(t) {
						if m.Event == "ai_otel_span" {
							spans++
						}
						if m.Event == "ai_llm_response_received" {
							calls++
						}
					}
					if spans != 1 || calls != 1 || len(llm.Requests(route)) != 1 {
						t.Fatalf("spans=%d calls=%d requests=%d", spans, calls, len(llm.Requests(route)))
					}
				})
			}
		}
	}
}
