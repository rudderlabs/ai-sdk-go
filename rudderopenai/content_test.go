package rudderopenai

import (
	"encoding/json"
	"testing"
)

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func req(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChatInputMessages(t *testing.T) {
	got := asJSON(t, chatInputMessages(req(t, `{"messages":[
		{"role":"developer","content":"be brief"},
		{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"https://x/y.png"}},{"type":"input_audio","input_audio":{"format":"wav"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		{"role":"assistant","function_call":{"name":"old","arguments":"{\"b\":2}"}},
		{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"ok"}]},
		{"role":"function","name":"old","content":"done"},
		{"role":"user","content":""}
	]}`)))
	want := `[{"role":"system","parts":[{"content":"be brief","type":"text"}]},` +
		`{"role":"user","parts":[{"content":"hi","type":"text"},{"mime_type":"image/png","modality":"image","type":"blob"},{"modality":"image","type":"uri","uri":"https://x/y.png"},{"mime_type":"wav","modality":"audio","type":"blob"}]},` +
		`{"role":"assistant","parts":[{"arguments":{"a":1},"id":"c1","name":"f","type":"tool_call"}]},` +
		`{"role":"assistant","parts":[{"arguments":{"b":2},"name":"old","type":"tool_call"}]},` +
		`{"role":"tool","parts":[{"id":"c1","response":"ok","type":"tool_call_response"}]},` +
		`{"role":"tool","parts":[{"id":"old","response":"done","type":"tool_call_response"}]}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestResponsesInputMessages(t *testing.T) {
	got := asJSON(t, responsesInputMessages(req(t, `{"instructions":"sys","input":[
		{"role":"developer","content":[{"type":"input_text","text":"d"},{"type":"input_image","image_url":"data:image/jpeg;base64,AA"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
		{"type":"function_call_output","call_id":"c1","output":"res"},
		{"type":"reasoning"}
	]}`)))
	want := `[{"role":"system","parts":[{"content":"sys","type":"text"}]},` +
		`{"role":"system","parts":[{"content":"d","type":"text"},{"mime_type":"image/jpeg","modality":"image","type":"blob"}]},` +
		`{"role":"assistant","parts":[{"arguments":{"a":1},"id":"c1","name":"f","type":"tool_call"}]},` +
		`{"role":"tool","parts":[{"id":"c1","response":"res","type":"tool_call_response"}]},` +
		`{"role":"assistant","parts":[{"type":"reasoning"}]}]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	str := asJSON(t, responsesInputMessages(req(t, `{"input":"hello"}`)))
	if str != `[{"role":"user","parts":[{"content":"hello","type":"text"}]}]` {
		t.Fatal(str)
	}
}

func TestComputerCallOutputScreenshot(t *testing.T) {
	got := responsesInputMessages(map[string]any{"input": []any{
		map[string]any{"type": "computer_call_output", "call_id": "c1", "output": map[string]any{"type": "computer_screenshot", "image_url": "data:image/png;base64,AAAA"}},
	}})
	if len(got) != 1 || got[0].Role != "tool" {
		t.Fatalf("%v", got)
	}
	resp := got[0].Parts[0]["response"].([]part)
	if resp[0]["type"] != "blob" || resp[0]["mime_type"] != "image/png" || resp[0]["uri"] != nil {
		t.Fatalf("%v", resp)
	}
}
