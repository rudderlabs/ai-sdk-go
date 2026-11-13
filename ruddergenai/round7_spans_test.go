package ruddergenai_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func TestLazyGoogleStreamCapturesOwnerAtStart(t *testing.T) {
	aliceClient := setup(t, rudderstackai.WithEmitSpans(true), rudderstackai.WithCaptureContent(true))
	bobClient := &recorder{}
	ctx, alice := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	ctx, parent := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "alice parent"})
	defer parent.End(nil)
	// Go's lazy iterator retains its explicit context argument; there is no implicit
	// interaction context to switch at entry. Ended owners are resolved at entry.
	ctx = rudderstackai.WithValues(ctx, rudderstackai.ContextValues{UserID: "bob", Analytics: bobClient})
	client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
	stream := client.Models.GenerateContentStream(ctx, model, genai.Text("Bob secret"), nil)
	alice.End(rudderstackai.EndOptions{})
	if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(false)); err != nil {
		t.Fatal(err)
	}
	llm.Script("google.stream", sseReply(reply([]any{m{"text": "Bob reply"}}, m{"promptTokenCount": 3, "candidatesTokenCount": 2}, "STOP")))
	for _, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
	}
	spans := 0
	for _, event := range bobClient.events(t) {
		if event.Event != "ai_otel_span" {
			continue
		}
		spans++
		if event.UserId != "bob" || event.Properties["interaction_id"] == alice.ID() || event.Properties["trace_id"] == parent.TraceID || event.Properties["parent_span_id"] != nil {
			t.Fatalf("creation owner retained: %v", event)
		}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "Bob secret") || strings.Contains(string(raw), "Bob reply") {
			t.Fatalf("entry capture policy lost: %s", raw)
		}
	}
	if spans != 1 {
		t.Fatalf("Bob spans=%d", spans)
	}
	for _, event := range aliceClient.events(t) {
		if event.Event == "ai_otel_span" {
			t.Fatal("invocation sent to creation client")
		}
	}
}
