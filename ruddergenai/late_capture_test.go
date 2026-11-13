package ruddergenai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

type lateCaptureTransport struct{ owner *rudderstackai.Interaction }

func (tr lateCaptureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	tr.owner.End(rudderstackai.EndOptions{})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"late_tool","args":{"value":"late argument"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`))}, nil
}
func TestLateGoogleResponseCapturesToolContent(t *testing.T) {
	rec := setup(t, rudderstackai.WithEmitSpans(true), rudderstackai.WithCaptureContent(true))
	ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	client := newClient(t, genai.BackendGeminiAPI, &http.Client{Transport: ruddergenai.Transport(lateCaptureTransport{owner})})
	if _, err := client.Models.GenerateContent(ctx, model, genai.Text("request"), nil); err != nil {
		t.Fatal(err)
	}
	for _, event := range rec.events(t) {
		if event.Event == "ai_otel_span" {
			raw, _ := json.Marshal(event)
			if !strings.Contains(string(raw), "late_tool") || !strings.Contains(string(raw), "late argument") {
				t.Fatalf("late content lost: %s", raw)
			}
			return
		}
	}
	t.Fatal("span missing")
}
