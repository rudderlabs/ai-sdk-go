package ruddergenai_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genai"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func TestFinalReviewGenAICancelledTwin(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			rec := setup(t, ai.WithEmitSpans(spans), ai.WithLLMCallEvents(true), ai.WithCredits(func(ai.CreditCall) (*float64, error) { return ai.Ptr(7.0), nil }))
			ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			started := time.Now()
			llm.Script("google.stream", sseReply(reply([]any{m{"text": "Hel"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 2}, ""), reply([]any{m{"text": "lo"}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5}, "STOP")))
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			for _, err := range client.Models.GenerateContentStream(ctx, model, genai.Text("Hi"), nil) {
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				break
			}
			// Submit the same call a processor reports through the OTel-free core hook.
			// Processor conversion and billing are covered in rudderotel's twin tests.
			hook.Tracing.RecordSpan(i, ai.LLMCall{
				Integration: "tracing", Provider: "gcp.gemini", Model: model, RequestedModel: model,
				InputTokens: 10, OutputTokens: 2, StartedAt: started, EndedAt: time.Now(),
			}, nil)
			i.End(ai.EndOptions{})
			response := rec.response(t)
			calls := 0
			for _, event := range rec.events(t) {
				if event.Event == "ai_llm_response_received" {
					calls++
					t.Logf("call=%v", event.Properties)
					if event.Properties["credits"] != nil {
						t.Fatal("cancelled call billed")
					}
				}
			}
			t.Logf("calls=%d response=%v", calls, response)
			if calls != 1 || response["total_credits"] != nil || response["response_status"] != "cancelled" {
				t.Fatal("cancelled wrapper/twin billed or miscounted")
			}
		})
	}
}
