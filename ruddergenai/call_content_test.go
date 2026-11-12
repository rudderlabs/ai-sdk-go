package ruddergenai

import (
	"encoding/json"
	"reflect"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInputMessages(t *testing.T) {
	req := decode(t, `{
	  "systemInstruction": {"parts": [{"text": "Be brief."}]},
	  "contents": [
	    {"role": "user", "parts": [{"text": "Hi"}, {"inlineData": {"mimeType": "image/png", "data": "AAAA"}}, {"fileData": {"mimeType": "application/pdf", "fileUri": "gs://b/f.pdf"}}]},
	    {"role": "model", "parts": [{"text": "thinking", "thought": true}, {"functionCall": {"id": "c1", "name": "weather", "args": {"city": "Paris"}}}]},
	    {"role": "user", "parts": [{"functionResponse": {"id": "c1", "name": "weather", "response": {"output": "sunny"}}}, {"text": "thanks"}]}
	  ]}`)
	want := []rudderstackai.GenAIMessage{
		{Role: "system", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "Be brief."}}},
		{Role: "user", Parts: []rudderstackai.GenAIPart{
			{"type": "text", "content": "Hi"},
			{"type": "blob", "modality": "image", "mime_type": "image/png"},
			{"type": "uri", "modality": "document", "uri": "gs://b/f.pdf", "mime_type": "application/pdf"},
		}},
		{Role: "assistant", Parts: []rudderstackai.GenAIPart{
			{"type": "reasoning", "content": "thinking"},
			{"type": "tool_call", "id": "c1", "name": "weather", "arguments": map[string]any{"city": "Paris"}},
		}},
		{Role: "tool", Parts: []rudderstackai.GenAIPart{{"type": "tool_call_response", "id": "c1", "response": map[string]any{"output": "sunny"}}}},
		{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": "thanks"}}},
	}
	if got := inputMessages(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestInputMessagesStringAndPartList(t *testing.T) {
	got := inputMessages(decode(t, `{"contents": ["a", {"text": "b"}, {"codeExecutionResult": {}}]}`))
	want := []rudderstackai.GenAIMessage{{Role: "user", Parts: []rudderstackai.GenAIPart{
		{"type": "text", "content": "a"}, {"type": "text", "content": "b"}, {"type": "codeExecutionResult"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if got := inputMessages(map[string]any{}); got != nil {
		t.Fatalf("empty request: %#v", got)
	}
}
