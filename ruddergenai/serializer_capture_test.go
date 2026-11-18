package ruddergenai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"google.golang.org/genai"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

type serializerCaptureTransport func(*http.Request) (*http.Response, error)

func (f serializerCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSerializerCapture(t *testing.T) {
	raw, err := os.ReadFile("../spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []serializerfixture.Case `json:"cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if !c.AppliesToGo() {
			continue
		}
		c, err := c.ResolveGo()
		if err != nil {
			t.Fatal(err)
		}
		late := c.Setup["scenario"] == "late_provider_response" && c.Setup["provider"] == "google"
		if !late && c.Setup["scenario"] != "request_control_timing" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			rec := setup(t, ai.WithEmitSpans(true), ai.WithCaptureContent(true))
			ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			config := &genai.GenerateContentConfig{Temperature: ai.Ptr(float32(.1)), ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"answer"}}}}
			before, _ := json.Marshal(config)
			transport := serializerCaptureTransport(func(r *http.Request) (*http.Response, error) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Error(readErr)
				}
				var wire map[string]any
				if decodeErr := json.Unmarshal(body, &wire); decodeErr != nil {
					t.Error(decodeErr)
				}
				generation, _ := wire["generationConfig"].(map[string]any)
				if generation["temperature"] != .1 || wire["toolConfig"] == nil {
					t.Errorf("typed controls changed: %s", body)
				}
				if late {
					owner.End(ai.EndOptions{})
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"DELIVERED-ANSWER"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`))}, nil
			})
			client := newClient(t, genai.BackendGeminiAPI, &http.Client{Transport: ruddergenai.Transport(transport)})
			invoke := func() {
				result, callErr := client.Models.GenerateContent(ctx, model, genai.Text("request"), config)
				if callErr != nil {
					t.Error(callErr)
					return
				}
				if result.Text() != "DELIVERED-ANSWER" {
					t.Errorf("app result=%q", result.Text())
				}
			}
			if c.Setup["mode"] == "async" {
				done := make(chan struct{})
				go func() { defer close(done); invoke() }()
				<-done
			} else {
				invoke()
			}
			after, _ := json.Marshal(config)
			if string(before) != string(after) {
				t.Fatal("app config mutated")
			}
			owner.End(ai.EndOptions{})
			if late {
				found := false
				for _, event := range rec.events(t) {
					if event.Event == "ai_otel_span" {
						b, _ := json.Marshal(event)
						found = strings.Contains(string(b), "DELIVERED-ANSWER")
					}
				}
				if !found {
					t.Fatal("late Google content missing")
				}
			}
		})
	}
}
