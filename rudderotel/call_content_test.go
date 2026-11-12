package rudderotel

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func attrs(kv ...attribute.KeyValue) map[string]attribute.Value { return attributes(kv) }

// asJSON normalises messages and calls so expectations read as JSON.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func wantJSON(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSpanCallContentConventions(t *testing.T) {
	str := attribute.String
	for _, c := range []struct {
		name         string
		attrs        map[string]attribute.Value
		input, calls string
	}{
		{
			"genai json strings with system instructions",
			attrs(
				str("gen_ai.system_instructions", `[{"type":"text","content":"Be brief."}]`),
				str("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"hi"}]},{"role":"assistant","parts":[{"type":"tool_call","id":"c0","name":"lookup","arguments":{"id":"1"}}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"c0","response":"shipped"}]}]`),
				str("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"ok"},{"type":"tool_call","name":"track","arguments":{"id":"2"}}]}]`),
			),
			`[{"role":"system","parts":[{"type":"text","content":"Be brief."}]},{"role":"user","parts":[{"type":"text","content":"hi"}]},{"role":"assistant","parts":[{"type":"tool_call","id":"c0","name":"lookup","arguments":{"id":"1"}}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"c0","response":"shipped"}]}]`,
			`[{"name":"track","arguments":{"id":"2"}}]`,
		},
		{
			"genai string slice and provider role names",
			attrs(
				attribute.StringSlice("gen_ai.input.messages", []string{`{"role":"human","parts":[{"type":"text","content":"a"}]}`, `{"role":"model","content":"b","tool_calls":[{"id":"t","function":{"name":"f","arguments":"{\"x\":1}"}}]}`}),
			),
			`[{"role":"user","parts":[{"type":"text","content":"a"}]},{"role":"assistant","parts":[{"type":"text","content":"b"},{"type":"tool_call","id":"t","name":"f","arguments":{"x":1}}]}]`,
			`null`,
		},
		{
			"genai media parts: uri, blob, reasoning, unknown",
			attrs(str("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"uri","modality":"image","uri":"https://x/y.png","mime_type":"image/png"},{"type":"blob","modality":"audio","mime_type":"audio/wav","content":"AAAA"},{"type":"thinking","text":"hm"},{"type":"video_x"}]}]`)),
			`[{"role":"user","parts":[{"type":"uri","modality":"image","uri":"https://x/y.png","mime_type":"image/png"},{"type":"blob","modality":"audio","mime_type":"audio/wav"},{"type":"reasoning","content":"hm"},{"type":"video_x"}]}]`,
			`null`,
		},
		{
			"legacy gen_ai.prompt.N and completion.N",
			attrs(
				str("gen_ai.prompt.0.role", "system"), str("gen_ai.prompt.0.content", "Be brief."),
				str("gen_ai.prompt.1.role", "user"), str("gen_ai.prompt.1.content", "Thanks"),
				str("gen_ai.prompt.2.role", "assistant"),
				str("gen_ai.prompt.2.tool_calls.0.id", "c1"), str("gen_ai.prompt.2.tool_calls.0.name", "f"), str("gen_ai.prompt.2.tool_calls.0.arguments", `{"a":1}`),
				str("gen_ai.prompt.3.role", "tool"), str("gen_ai.prompt.3.tool_call_id", "c1"), str("gen_ai.prompt.3.content", "done"),
				str("gen_ai.completion.0.role", "assistant"),
				str("gen_ai.completion.0.tool_calls.0.name", "next"), str("gen_ai.completion.0.tool_calls.0.arguments", `{"b":2}`),
			),
			`[{"role":"system","parts":[{"type":"text","content":"Be brief."}]},{"role":"user","parts":[{"type":"text","content":"Thanks"}]},{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"f","arguments":{"a":1}}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"c1","response":"done"}]}]`,
			`[{"name":"next","arguments":{"b":2}}]`,
		},
		{
			"openinference messages, contents and data URI image",
			attrs(
				str("llm.input_messages.0.message.role", "user"),
				str("llm.input_messages.0.message.contents.0.message_content.type", "text"),
				str("llm.input_messages.0.message.contents.0.message_content.text", "look"),
				str("llm.input_messages.0.message.contents.1.message_content.type", "image"),
				str("llm.input_messages.0.message.contents.1.message_content.image.image.url", "data:image/png;base64,AAAA"),
				str("llm.input_messages.0.message.contents.2.message_content.type", "image"),
				str("llm.input_messages.0.message.contents.2.message_content.image.image.url", "https://x/y.png"),
				str("llm.input_messages.1.message.role", "tool"),
				str("llm.input_messages.1.message.tool_call_id", "c9"),
				str("llm.input_messages.1.message.content", "result"),
				str("llm.output_messages.0.message.role", "assistant"),
				str("llm.output_messages.0.message.tool_calls.0.tool_call.id", "c10"),
				str("llm.output_messages.0.message.tool_calls.0.tool_call.function.name", "close"),
				str("llm.output_messages.0.message.tool_calls.0.tool_call.function.arguments", `{"id":"1234"}`),
			),
			`[{"role":"user","parts":[{"type":"text","content":"look"},{"type":"blob","modality":"image","mime_type":"image/png"},{"type":"uri","modality":"image","uri":"https://x/y.png"}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"c9","response":"result"}]}]`,
			`[{"name":"close","arguments":{"id":"1234"}}]`,
		},
		{
			"system message already leading the input is not repeated",
			attrs(
				str("gen_ai.system_instructions", "Be brief."),
				str("gen_ai.input.messages", `[{"role":"system","parts":[{"type":"text","content":"Be brief."}]},{"role":"user","parts":[{"type":"text","content":"hi"}]}]`),
			),
			`[{"role":"system","parts":[{"type":"text","content":"Be brief."}]},{"role":"user","parts":[{"type":"text","content":"hi"}]}]`,
			`null`,
		},
		{
			"plain string system instructions alone",
			attrs(str("gen_ai.system_instructions", "Be brief.")),
			`[{"role":"system","parts":[{"type":"text","content":"Be brief."}]}]`,
			`null`,
		},
		{"no content attributes", attrs(str("gen_ai.request.model", "gpt-5")), `null`, `null`},
	} {
		t.Run(c.name, func(t *testing.T) {
			input, calls := spanCallContent(c.attrs)
			if got, want := asJSON(t, input), wantJSON(t, c.input); !reflect.DeepEqual(got, want) {
				t.Errorf("input_messages\n got %v\nwant %v", got, want)
			}
			if got, want := asJSON(t, calls), wantJSON(t, c.calls); !reflect.DeepEqual(got, want) {
				t.Errorf("output_tool_calls\n got %v\nwant %v", got, want)
			}
		})
	}
}
