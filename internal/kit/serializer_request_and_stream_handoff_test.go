package kit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ai "github.com/rudderlabs/ai-sdk-go"
)

type executingSerializer struct {
	calls int
	fail  bool
	text  string
}

func (v *executingSerializer) MarshalJSON() ([]byte, error) {
	v.calls++
	if v.fail {
		return nil, fmt.Errorf("serializer failure")
	}
	return json.Marshal(map[string]any{"public": v.text})
}
func TestExecutingRequestHandoff(t *testing.T) {
	for _, provider := range []string{"openai-chat", "openai-responses", "anthropic", "google"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, stream), func(t *testing.T) {
				r := reviewRecorder(t)
				if err := ai.Configure(ai.WithCaptureContent(true)); err != nil {
					t.Fatal(err)
				}
				ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
				app := &executingSerializer{text: "SENT"}
				raw, err := json.Marshal(map[string]any{"model": "m", "message": app})
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid", strings.NewReader(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				spec := Spec{Provider: provider, Integration: provider, InputMessages: func(body map[string]any) []ai.GenAIMessage {
					return []ai.GenAIMessage{{Role: "user", Parts: []ai.GenAIPart{{"type": "text", "content": body["message"]}}}}
				}, IsStream: func(*http.Request, map[string]any) bool { return stream }, OnResponse: func(c *Call, _ map[string]any, _ *http.Response) bool {
					return c.RecordCall(ai.LLMCall{Provider: provider, Model: "m", InputTokens: 2, OutputTokens: 1})
				}, NewStream: func(c *Call, _ *http.Response) StreamHandler {
					o := NewObserver(c)
					o.InputTokens = ai.Ptr(2)
					o.OutputTokens = ai.Ptr(1)
					o.Done = true
					return &chatStream{Observer: o}
				}}
				resp, err := spec.Intercept(req, func(sent *http.Request) (*http.Response, error) {
					body, e := io.ReadAll(sent.Body)
					if e != nil || string(body) != string(raw) {
						t.Fatalf("sent=%s err=%v", body, e)
					}
					app.text = "PRIVATE_ADDED"
					contentType := "application/json"
					bodyText := "{}"
					if stream {
						contentType = "text/event-stream"
						bodyText = ""
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(bodyText))}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				owner.End(ai.EndOptions{})
				events := r.named("ai_llm_response_received")
				if len(events) != 1 {
					t.Fatalf("calls=%d", len(events))
				}
				got, _ := json.Marshal(events[0])
				if !strings.Contains(string(got), "SENT") || strings.Contains(string(got), "PRIVATE_ADDED") || app.calls != 1 {
					t.Fatalf("capture=%s serializers=%d", got, app.calls)
				}
			})
		}
	}
}
func TestExecutingStreamSerializerSiblings(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("capture=%t/fail=%t", capture, fail), func(t *testing.T) {
				r := reviewRecorder(t)
				if err := ai.Configure(ai.WithEmitSpans(capture), ai.WithCaptureContent(capture)); err != nil {
					t.Fatal(err)
				}
				_, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
				v := &executingSerializer{text: "visible", fail: fail}
				o := NewObserver(&Call{Provider: "openai", Integration: "review", Model: "m", Interaction: owner, StartedAt: time.Now()})
				o.NoteTool("t", "t", "lookup", v, "")
				o.InputTokens = ai.Ptr(2)
				o.OutputTokens = ai.Ptr(1)
				o.Done = true
				o.Finish(Completed, "")
				owner.End(ai.EndOptions{})
				if !capture && v.calls != 0 {
					t.Fatalf("disabled stream serialized %d times", v.calls)
				}
				events := r.named("ai_llm_response_received")
				if len(events) != 1 || events[0].Properties["input_tokens"] != 2 {
					t.Fatalf("lost stream accounting %v", events)
				}
				if capture {
					calls := events[0].Properties["output_tool_calls"].([]any)
					args := calls[0].(map[string]any)["arguments"]
					if fail && args != nil {
						t.Fatalf("failed args=%v", args)
					}
					if !fail && args.(map[string]any)["public"] != "visible" {
						t.Fatal("serializer projection lost")
					}
				}
			})
		}
	}
}
