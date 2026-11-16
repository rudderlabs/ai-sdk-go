package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestStructuredOutputProviders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testkit.RepoRoot(), "spec/fixtures/structured-output-v0.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Provider string
			Request        obj
			Response       json.RawMessage   `json:"provider_response"`
			Stream         []json.RawMessage `json:"provider_stream"`
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if c.Provider == "" || c.Provider == "vercel" {
			continue
		}
		for _, stream := range []bool{false, true} {
			if stream && len(c.Stream) == 0 {
				continue
			}
			mode := "sync"
			if stream {
				mode = "stream"
			}
			t.Run(c.Name+"/"+mode, func(t *testing.T) {
				rec := &recorder{}
				configure(t, obj{"tool_breakdown": true, "emit_llm_calls": true, "event_rules": obj{"tools": obj{"Schema": "schema_done"}}}, rec)
				llm.Reset()
				params := obj{"model": "gpt-4o", "messages": []any{}, "contents": []any{}, "modelId": "claude-sonnet-4-5"}
				for k, v := range c.Request {
					params[k] = v
				}
				step := obj{"params": params}
				route := map[string]string{"openai-chat": "openai.chat", "openai-responses": "openai.responses", "anthropic": "anthropic.messages", "gemini": "google.generate", "bedrock": "bedrock.converse"}[c.Provider]
				if stream {
					params["stream"] = true
					if c.Provider == "gemini" {
						route = "google.stream"
					}
					if c.Provider == "bedrock" {
						route = "bedrock.converse_stream"
					}
				}
				if c.Provider == "bedrock" {
					obj0(params["toolConfig"])["tools"] = []any{obj{"toolSpec": obj{"name": "Schema", "inputSchema": obj{"json": obj{"type": "object"}}}}}
					step["response"] = c.Response
					if stream {
						step["stream"] = c.Stream
					}
				} else {
					reply := testkit.Reply{JSON: c.Response}
					if stream {
						reply.JSON = nil
						for _, frame := range c.Stream {
							var part obj
							if err := json.Unmarshal(frame, &part); err != nil {
								t.Fatal(err)
							}
							event := ""
							if c.Provider == "anthropic" || c.Provider == "openai-responses" {
								event = str(part["type"])
							}
							reply.SSE = append(reply.SSE, testkit.SSEEvent{Event: event, Data: frame})
						}
					}
					llm.Script(route, reply)
				}
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
				r := &run{t: t, llm: llm, cur: i, last: i}
				if err := apis[route](ctx, r, step); err != nil {
					t.Fatal(err)
				}
				i.End(rudderstackai.EndOptions{})
				events := rec.all()
				calls := 0
				for _, e := range events {
					switch e.Event {
					case "ai_llm_response_received":
						calls++
					case "schema_done":
						t.Error("structured output emitted a tool event")
					case "ai_response_received":
						for key, want := range map[string]float64{"tool_call_count": 0, "llm_call_count": 1, "total_input_tokens": 10, "total_output_tokens": 5} {
							if structuredNumber(e.Properties[key]) != want {
								t.Errorf("%s = %v; want %v", key, e.Properties[key], want)
							}
						}
						if _, ok := e.Properties["tool_breakdown"]; ok {
							t.Error("structured output has tool breakdown")
						}
					}
				}
				if calls != 1 {
					t.Errorf("model events = %d; want 1", calls)
				}
			})
		}
	}
}

func structuredNumber(v any) float64 {
	if n, ok := v.(int); ok {
		return float64(n)
	}
	return num(v)
}
