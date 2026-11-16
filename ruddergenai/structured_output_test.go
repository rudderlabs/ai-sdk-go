package ruddergenai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

func schemaConfig() *genai.GenerateContentConfig {
	return &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "Schema"}}}},
		ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"Schema"},
		}},
	}
}

func sendChat(ctx context.Context, chat *genai.Chat, stream bool, part genai.Part) error {
	if stream {
		for _, err := range chat.SendMessageStream(ctx, part) {
			if err != nil {
				return err
			}
		}
		return nil
	}
	_, err := chat.SendMessage(ctx, part)
	return err
}

func schemaEvents(t *testing.T, rec *recorder) int {
	t.Helper()
	count := 0
	for _, event := range rec.events(t) {
		if event.Event == "schema_done" {
			count++
		}
	}
	return count
}

func TestIdlessForcedGeminiNamedResult(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, forced := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/forced=%t", stream, forced), func(t *testing.T) {
				rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{
					Tools: map[string]rudderstackai.ToolEventRule{"Schema": {Event: "schema_done"}},
				}))
				call := reply([]any{m{"functionCall": m{"name": "Schema", "args": m{"answer": "yes"}}}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5}, "STOP")
				answer := reply([]any{m{"text": "Done."}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5}, "STOP")
				answer["responseId"] = "r2"
				if stream {
					llm.Script("google.stream", sseReply(call), sseReply(answer))
				} else {
					llm.Script("google.generate", jsonReply(call), jsonReply(answer))
				}
				client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
				config := schemaConfig()
				if !forced {
					config.ToolConfig = nil
				}
				if err := interaction(t, func(ctx context.Context) error {
					chat, err := client.Chats.Create(ctx, model, config, nil)
					if err != nil {
						return err
					}
					if err := sendChat(ctx, chat, stream, genai.Part{Text: "Answer?"}); err != nil {
						return err
					}
					return sendChat(ctx, chat, stream, genai.Part{FunctionResponse: &genai.FunctionResponse{
						Name: "Schema", Response: m{"output": m{"answer": "yes"}},
					}})
				}); err != nil {
					t.Fatal(err)
				}
				props := rec.response(t)
				want(t, props, "tool_call_count", 1)
				row := m{"tool_name": "Schema", "call_count": 1}
				if forced {
					row["error_count"] = 0
				}
				want(t, props, "tool_breakdown", []any{row})
				want(t, props, "llm_call_count", 2)
				if got := schemaEvents(t, rec); got != 1 {
					t.Fatalf("events = %d; want 1", got)
				}
			})
		}
	}
}

func TestGeminiChatActualOutgoingConfig(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{
				Tools: map[string]rudderstackai.ToolEventRule{"Schema": {Event: "schema_done"}},
			}))
			response := reply([]any{m{"functionCall": m{"id": "schema", "name": "Schema", "args": m{}}}}, m{"promptTokenCount": 10, "candidatesTokenCount": 5}, "STOP")
			route := "google.generate"
			if stream {
				route = "google.stream"
				llm.Script(route, sseReply(response))
			} else {
				llm.Script(route, jsonReply(response))
			}
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			if err := interaction(t, func(ctx context.Context) error {
				config := schemaConfig()
				chat, err := client.Chats.Create(ctx, model, config, nil)
				if err != nil {
					return err
				}
				// Go chat holds the config pointer. Replace its contents before the send
				// to exercise the outgoing override represented by Node's per-send config.
				*config = genai.GenerateContentConfig{Tools: config.Tools, Temperature: rudderstackai.Ptr(float32(0.2))}
				return sendChat(ctx, chat, stream, genai.Part{Text: "Answer?"})
			}); err != nil {
				t.Fatal(err)
			}
			requests := llm.Requests(route)
			if len(requests) != 1 {
				t.Fatalf("requests = %d; want 1", len(requests))
			}
			var body m
			if err := json.Unmarshal(requests[0], &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["toolConfig"]; ok {
				t.Fatalf("outgoing request inherited tool choice: %s", requests[0])
			}
			if body["tools"] == nil {
				t.Fatal("outgoing request missing tools")
			}
			want(t, body["generationConfig"].(map[string]any), "temperature", 0.2)
			want(t, rec.response(t), "tool_call_count", 1)
			if got := schemaEvents(t, rec); got != 1 {
				t.Fatalf("events = %d; want 1", got)
			}
		})
	}
}
