package ruddergenai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func TestExecutingHostedCodeExecution(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("spans=%t/stream=%t", spans, stream), func(t *testing.T) {
				rec := setup(t, ai.WithEmitSpans(spans), ai.WithCaptureContent(true), ai.WithLLMCallEvents(true))
				body := reply([]any{m{"executableCode": m{"code": "1"}}, m{"text": "done"}}, m{"promptTokenCount": 2, "candidatesTokenCount": 1}, "STOP")
				if stream {
					llm.Script("google.stream", sseReply(body))
				} else {
					llm.Script("google.generate", jsonReply(body))
				}
				client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
				ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
				if stream {
					for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
						if err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil); err != nil {
						t.Fatal(err)
					}
				}
				owner.End(ai.EndOptions{})
				calls, spanCount := 0, 0
				for _, event := range rec.events(t) {
					wire, _ := json.Marshal(event.Properties)
					if event.Event == "ai_llm_response_received" {
						calls++
						if !strings.Contains(string(wire), `"name":"code_execution"`) {
							t.Fatalf("hosted tool missing: %s", wire)
						}
					}
					if event.Event == "ai_otel_span" {
						spanCount++
						if !strings.Contains(string(wire), "code_execution") {
							t.Fatal("hosted tool absent from span")
						}
					}
				}
				if calls != 1 || spans && spanCount != 1 {
					t.Fatalf("calls=%d spans=%d", calls, spanCount)
				}
			})
		}
	}
}
