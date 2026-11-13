package ruddergenai_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func TestGoogleSDKRetriesOneInvocationSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(fmt.Sprint(inside), func(t *testing.T) {
			rec := setup(t, rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true))
			llm.Script("google.generate", testkit.Reply{Status: 503, JSON: []byte(`{"error":{"code":503,"message":"busy"}}`)}, jsonReply(map[string]any{"responseId": "retry-answer", "modelVersion": model, "candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "ok"}}}, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"promptTokenCount": 2, "candidatesTokenCount": 1}}))
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			ctx := context.Background()
			var i *rudderstackai.Interaction
			if inside {
				ctx, i = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			}
			_, err := client.Models.GenerateContent(ctx, model, genai.Text("hello"), &genai.GenerateContentConfig{HTTPOptions: &genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{Attempts: rudderstackai.Ptr(int32(2)), InitialDelay: rudderstackai.Ptr(0.001)}}})
			if err != nil {
				t.Fatal(err)
			}
			if i != nil {
				i.End(rudderstackai.EndOptions{})
			}
			if got := len(llm.Requests("google.generate")); got != 2 {
				t.Fatalf("attempts=%d", got)
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
			if spans != 1 || calls != 1 {
				t.Fatalf("spans=%d calls=%d", spans, calls)
			}
		})
	}
}

func TestGoogleSpansOffRetriesMatchMain(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(fmt.Sprint(inside), func(t *testing.T) {
			rec := setup(t, rudderstackai.WithEmitSpans(false), rudderstackai.WithLLMCallEvents(true))
			llm.Script("google.generate", testkit.Reply{Status: 503, JSON: []byte(`{"error":{"code":503,"message":"busy"}}`)}, jsonReply(reply([]any{m{"text": "ok"}}, m{"promptTokenCount": 2, "candidatesTokenCount": 1}, "STOP")))
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			ctx := context.Background()
			var owner *rudderstackai.Interaction
			if inside {
				ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice"})
			}
			_, err := client.Models.GenerateContent(ctx, model, genai.Text("hello"), &genai.GenerateContentConfig{HTTPOptions: &genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{Attempts: rudderstackai.Ptr(int32(2)), InitialDelay: rudderstackai.Ptr(0.001)}}})
			if err != nil {
				t.Fatal(err)
			}
			if owner != nil {
				owner.End(rudderstackai.EndOptions{})
			}
			calls := 0
			for _, event := range rec.events(t) {
				if event.Event == "ai_llm_response_received" {
					calls++
				}
				if event.Event == "ai_otel_span" {
					t.Fatal("disabled span")
				}
			}
			if calls != 2 {
				t.Fatalf("calls=%d, origin/main records 2", calls)
			}
		})
	}
}
