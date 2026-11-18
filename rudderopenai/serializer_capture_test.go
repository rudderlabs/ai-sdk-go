package rudderopenai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

// Go providers receive wire-decoded content. Exercise the real middleware boundary,
// including the original request controls and response pointer, rather than replacing
// the provider's typed result with a fixture-only serializer object.
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
		scenario := c.Setup["scenario"]
		if scenario == "request_handoff" {
			t.Run(c.ID, func(t *testing.T) { runRequestHandoff(t, c) })
			continue
		}
		late := scenario == "late_provider_response" && c.Setup["provider"] == "openai_chat"
		if !late && scenario != "app_result_identity" && scenario != "request_control_timing" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			rec, _ := setup(t, ai.WithEmitSpans(true), ai.WithCaptureContent(true))
			ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			requestBody := `{"model":"m","messages":[{"role":"user","content":"request"}],"tool_choice":{"type":"function","function":{"name":"answer"}},"tools":[{"type":"function","function":{"name":"answer","parameters":{"type":"object"}}}]}`
			responseBody := `{"id":"late","model":"m","choices":[{"message":{"role":"assistant","content":"DELIVERED-ANSWER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
			original := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody))}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/chat/completions", strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			invoke := func() {
				received, callErr := rudderopenai.Middleware()(request, func(out *http.Request) (*http.Response, error) {
					body, readErr := io.ReadAll(out.Body)
					if readErr != nil {
						t.Error(readErr)
					}
					if string(body) != requestBody {
						t.Errorf("request controls changed: %s", body)
					}
					if late {
						owner.End(ai.EndOptions{})
					}
					return original, nil
				})
				if callErr != nil || received != original {
					t.Errorf("response replaced: same=%v err=%v", received == original, callErr)
					return
				}
				body, readErr := io.ReadAll(received.Body)
				if readErr != nil || string(body) != responseBody {
					t.Errorf("app response changed: %s err=%v", body, readErr)
				}
				_ = received.Body.Close()
			}
			if c.Setup["mode"] == "async" {
				done := make(chan struct{})
				go func() { defer close(done); invoke() }()
				<-done
			} else {
				invoke()
			}
			owner.End(ai.EndOptions{})
			if late {
				found := false
				for _, event := range rec.events(t) {
					if event.name == "ai_otel_span" {
						b, _ := json.Marshal(event.props)
						found = strings.Contains(string(b), "DELIVERED-ANSWER")
					}
				}
				if !found {
					t.Fatal("late provider response content missing")
				}
			}
		})
	}
}
