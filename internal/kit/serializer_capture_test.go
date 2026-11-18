package kit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
)

type wireFixtureValue struct {
	output any
	calls  int
}

func (v *wireFixtureValue) MarshalJSON() ([]byte, error) { v.calls++; return json.Marshal(v.output) }

type wireFixtureStream struct{ *Observer }

func (s *wireFixtureStream) OnEvent(_ string, data []byte) {
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	message, _ := value["message"].(map[string]any)
	text, _ := message["content"].(string)
	s.NoteText(text)
	s.InputTokens = ai.Ptr(2)
	s.OutputTokens = ai.Ptr(1)
	s.Done = true
}

// The provider matrix uses real HTTP and SSE interception. Application serializers run
// in the simulated provider encoding; Go telemetry reads the resulting wire values.
func TestSerializerCapture(t *testing.T) {
	raw, err := os.ReadFile("../../spec/fixtures/serializer-capture-v0.1.json")
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
		site := c.Setup["site"]
		if !c.AppliesToGo() || !strings.HasPrefix(c.ID, "P1-") || (site != "provider_request_message" && site != "provider_response_message") {
			continue
		}
		c, err = c.ResolveGo()
		if err != nil {
			t.Fatal(err)
		}
		t.Run(c.ID, func(t *testing.T) {
			rec := reviewRecorder(t)
			if err := ai.Configure(ai.WithEmitSpans(c.Config.EmitSpans), ai.WithCaptureContent(c.Config.CaptureContent), ai.WithRedaction(c.Config.Redact)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			descriptor := c.Setup["value"].(map[string]any)
			v := &wireFixtureValue{output: descriptor["serialized"]}
			var requestMessage any = map[string]any{"role": "user", "content": "request"}
			if site == "provider_request_message" {
				requestMessage = v
			}
			requestRaw, err := json.Marshal(map[string]any{"model": "m", "messages": []any{requestMessage}})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid", strings.NewReader(string(requestRaw)))
			if err != nil {
				t.Fatal(err)
			}
			stream, _ := c.Setup["stream"].(bool)
			spec := Spec{Provider: "openai", Integration: "openai", IsStream: func(*http.Request, map[string]any) bool { return stream }, InputMessages: func(body map[string]any) []ai.GenAIMessage {
				var messages []ai.GenAIMessage
				for _, item := range body["messages"].([]any) {
					m := item.(map[string]any)
					messages = append(messages, ai.GenAIMessage{Role: m["role"].(string), Parts: []ai.GenAIPart{{"type": "text", "content": m["content"]}}})
				}
				return messages
			}, OnResponse: func(call *Call, body map[string]any, _ *http.Response) bool {
				message, _ := body["message"].(map[string]any)
				text, _ := message["content"].(string)
				return call.RecordCall(ai.LLMCall{Provider: "openai", Model: "m", InputTokens: 2, OutputTokens: 1, OutputText: text})
			}, NewStream: func(call *Call, _ *http.Response) StreamHandler { return &wireFixtureStream{NewObserver(call)} }}
			parentCaller := fixtureCaller()
			invoke := func() {
				async := c.Setup["mode"] == "async"
				if (fixtureCaller() != parentCaller) != async {
					t.Error("fixture executed on the wrong goroutine")
					return
				}
				response, err := spec.Intercept(request, func(sent *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(sent.Body)
					if err != nil || string(body) != string(requestRaw) {
						t.Errorf("request changed: %s %v", body, err)
					}
					var message any = map[string]any{"role": "assistant", "content": "answer"}
					if site == "provider_response_message" {
						message = v
					}
					responseRaw, err := json.Marshal(map[string]any{"message": message})
					if err != nil {
						return nil, err
					}
					text := string(responseRaw)
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
						text = "data: " + text + "\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(text))}, nil
				})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err = io.ReadAll(response.Body); err != nil {
					t.Error(err)
				}
				_ = response.Body.Close()
			}
			if c.Setup["mode"] == "async" {
				done := make(chan struct{})
				go func() { defer close(done); invoke() }()
				<-done
			} else {
				invoke()
			}
			owner.End(ai.EndOptions{})
			events := rec.named("ai_llm_response_received")
			if len(events) != 1 {
				t.Fatalf("calls=%d", len(events))
			}
			p := events[0].Properties
			var got any
			if site == "provider_request_message" {
				got = p["input_messages"].([]any)[0]
			} else {
				got = map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": p["output_text"]}}}
			}
			actual, _ := json.Marshal(got)
			expected, _ := json.Marshal(c.Expect["decoded"].(map[string]any)["captured_value"])
			if string(actual) != string(expected) {
				t.Fatalf("captured=%s want=%s", actual, expected)
			}
			want := 1
			if counts, ok := c.Expect["serializer_calls"].(map[string]any); ok {
				want = int(counts["value"].(float64))
			}
			if v.calls != want {
				t.Fatalf("serializers=%d want=%d", v.calls, want)
			}
			if c.Config.EmitSpans && len(rec.named("ai_otel_span")) != 1 {
				t.Fatal("stream span missing")
			}
			all, _ := json.Marshal(rec.named("ai_llm_response_received"))
			for _, word := range c.Expect["never_emitted"].([]any) {
				if strings.Contains(string(all), fmt.Sprint(word)) {
					t.Fatalf("emitted %v", word)
				}
			}
		})
	}
}

func fixtureCaller() string {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}
