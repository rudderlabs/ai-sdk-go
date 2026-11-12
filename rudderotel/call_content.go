package rudderotel

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

// Per-call content (input_messages, output_tool_calls) of a model-call span, for productEvents.
// Core masks, caps and gates the result; spanCallContent runs only when the interaction captures it.

type part = rudderstackai.GenAIPart

// rawAttr is the attribute as a generic value; nil when absent. String slices become []any.
func rawAttr(a map[string]attribute.Value, key string) any {
	v, ok := a[key]
	if !ok {
		return nil
	}
	if v.Type() == attribute.STRINGSLICE {
		out := []any{}
		for _, s := range v.AsStringSlice() {
			out = append(out, s)
		}
		return out
	}
	return v.AsInterface()
}

// roleOf maps provider role names onto the four GenAI roles; "" when absent.
func roleOf(role any) string {
	s, _ := role.(string)
	switch r := strings.ToLower(s); r {
	case "developer", "instructions":
		return "system"
	case "model", "ai":
		return "assistant"
	case "function":
		return "tool"
	case "human":
		return "user"
	default:
		return r
	}
}

func roleOr(role any, fallback string) string {
	if r := roleOf(role); r != "" {
		return r
	}
	return fallback
}

// parsedJSON decodes a JSON string (nil when invalid); a list is decoded element-wise.
func parsedJSON(v any) any {
	switch x := v.(type) {
	case string:
		var out any
		if json.Unmarshal([]byte(x), &out) != nil {
			return nil
		}
		return out
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			if s, ok := e.(string); ok {
				out[n] = parsedJSON(s)
			} else {
				out[n] = e
			}
		}
		return out
	}
	return v
}

func coalesce(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func modality(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "image"
}

// genAIPart is one GenAI semconv part ({type, content | id, name, arguments | modality, uri ...}).
func genAIPart(p any) []part {
	if s, ok := p.(string); ok {
		return kit.TextPart(s)
	}
	m, ok := p.(map[string]any)
	if !ok {
		return nil
	}
	switch m["type"] {
	case "text":
		return kit.TextPart(coalesce(m["content"], m["text"]))
	case "reasoning", "thinking":
		return kit.ReasoningPart(coalesce(m["content"], m["text"]))
	case "tool_call":
		return kit.ToolCallPart(m["id"], m["name"], m["arguments"])
	case "tool_call_response":
		return []part{kit.ToolResponsePart(m["id"], coalesce(m["response"], m["result"]))}
	case "uri":
		return kit.URIPart(modality(m["modality"]), m["uri"], m["mime_type"])
	case "blob":
		return []part{kit.BlobPart(modality(m["modality"]), m["mime_type"])}
	default:
		return kit.OtherPart(m["type"])
	}
}

func flatParts(list []any) []part {
	var out []part
	for _, e := range list {
		out = append(out, genAIPart(e)...)
	}
	return out
}

// legacyToolCalls are the tool calls of the older {role, content, tool_calls} shape (OpenAI-style or flat).
func legacyToolCalls(v any) []part {
	list, _ := v.([]any)
	var out []part
	for _, e := range list {
		t, ok := e.(map[string]any)
		if !ok {
			continue
		}
		fn := t
		if f, ok := t["function"].(map[string]any); ok {
			fn = f
		}
		out = append(out, kit.ToolCallPart(t["id"], fn["name"], fn["arguments"])...)
	}
	return out
}

// contentParts is a content that is a string or a list of text blocks.
func contentParts(content any) []part {
	if s, ok := content.(string); ok {
		return kit.TextPart(s)
	}
	if list, ok := content.([]any); ok {
		return flatParts(list)
	}
	return nil
}

// genAIMessage is one message of gen_ai.input.messages / gen_ai.output.messages, in either shape.
func genAIMessage(v any) rudderstackai.GenAIMessage {
	m, ok := v.(map[string]any)
	if !ok {
		return rudderstackai.GenAIMessage{}
	}
	role := roleOr(m["role"], "assistant")
	if list, ok := m["parts"].([]any); ok {
		return kit.Message(role, flatParts(list)...)
	}
	if role == "tool" && m["content"] != nil {
		return kit.Message(role, kit.ToolResponsePart(m["tool_call_id"], m["content"]))
	}
	return kit.Message(role, kit.Parts(contentParts(m["content"]), legacyToolCalls(m["tool_calls"]))...)
}

func genAIMessages(v any) []rudderstackai.GenAIMessage {
	list, ok := parsedJSON(v).([]any)
	if !ok {
		return nil
	}
	msgs := make([]rudderstackai.GenAIMessage, len(list))
	for n, e := range list {
		msgs[n] = genAIMessage(e)
	}
	return kit.Messages(msgs...)
}

// attrIndexes are the sorted distinct N of the attribute keys <prefix>.N.…
func attrIndexes(a map[string]attribute.Value, prefix string) []int {
	found := map[int]bool{}
	for k := range a {
		rest, ok := strings.CutPrefix(k, prefix+".")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, ".")
		if n, err := strconv.Atoi(first); err == nil {
			found[n] = true
		}
	}
	out := make([]int, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// legacyMessage is message n of the OpenLLMetry gen_ai.prompt.N.* / gen_ai.completion.N.* form.
func legacyMessage(a map[string]attribute.Value, base string, n int) rudderstackai.GenAIMessage {
	at := base + "." + strconv.Itoa(n)
	fallback := "user"
	if strings.HasSuffix(base, "completion") {
		fallback = "assistant"
	}
	role := roleOr(rawAttr(a, at+".role"), fallback)
	content := rawAttr(a, at+".content")
	if role == "tool" && content != nil {
		return kit.Message(role, kit.ToolResponsePart(rawAttr(a, at+".tool_call_id"), content))
	}
	var calls []part
	for _, i := range attrIndexes(a, at+".tool_calls") {
		c := at + ".tool_calls." + strconv.Itoa(i)
		calls = append(calls, kit.ToolCallPart(rawAttr(a, c+".id"), rawAttr(a, c+".name"), rawAttr(a, c+".arguments"))...)
	}
	return kit.Message(role, kit.Parts(kit.TextPart(asStringAny(content)), calls)...)
}

// asStringAny is the content when it is a string, else "".
func asStringAny(v any) string {
	s, _ := v.(string)
	return s
}

// openInferenceMessage is message n of the llm.{input,output}_messages.N.message.* form.
func openInferenceMessage(a map[string]attribute.Value, base string, n int) rudderstackai.GenAIMessage {
	at := base + "." + strconv.Itoa(n) + ".message"
	fallback := "user"
	if strings.Contains(base, "output") {
		fallback = "assistant"
	}
	role := roleOr(rawAttr(a, at+".role"), fallback)
	content := rawAttr(a, at+".content")
	if role == "tool" {
		return kit.Message(role, kit.ToolResponsePart(rawAttr(a, at+".tool_call_id"), coalesce(content, rawAttr(a, at+".contents.0.message_content.text"))))
	}
	var blocks, calls []part
	for _, i := range attrIndexes(a, at+".contents") {
		c := at + ".contents." + strconv.Itoa(i) + ".message_content"
		typ := asStringAny(rawAttr(a, c+".type"))
		switch typ {
		case "image":
			blocks = append(blocks, kit.URIPart("image", rawAttr(a, c+".image.image.url"), nil)...)
		case "", "text":
			blocks = append(blocks, kit.TextPart(rawAttr(a, c+".text"))...)
		default:
			blocks = append(blocks, kit.OtherPart(typ)...)
		}
	}
	for _, i := range attrIndexes(a, at+".tool_calls") {
		t := at + ".tool_calls." + strconv.Itoa(i) + ".tool_call"
		calls = append(calls, kit.ToolCallPart(rawAttr(a, t+".id"), rawAttr(a, t+".function.name"), rawAttr(a, t+".function.arguments"))...)
	}
	return kit.Message(role, kit.Parts(kit.TextPart(asStringAny(content)), blocks, calls)...)
}

func indexedMessages(a map[string]attribute.Value, base string, build func(map[string]attribute.Value, string, int) rudderstackai.GenAIMessage) []rudderstackai.GenAIMessage {
	var msgs []rudderstackai.GenAIMessage
	for _, n := range attrIndexes(a, base) {
		msgs = append(msgs, build(a, base, n))
	}
	return kit.Messages(msgs...)
}

// inputOf reads the first convention the span carries: GenAI, legacy, then OpenInference.
func inputOf(a map[string]attribute.Value) []rudderstackai.GenAIMessage {
	if m := genAIMessages(rawAttr(a, "gen_ai.input.messages")); len(m) > 0 {
		return m
	}
	if m := indexedMessages(a, "gen_ai.prompt", legacyMessage); len(m) > 0 {
		return m
	}
	return indexedMessages(a, "llm.input_messages", openInferenceMessage)
}

// outputOf reads the same conventions in order; only the first choice for the indexed ones.
func outputOf(a map[string]attribute.Value) []rudderstackai.GenAIMessage {
	if m := genAIMessages(rawAttr(a, "gen_ai.output.messages")); len(m) > 0 {
		return m
	}
	if m := kit.Messages(legacyMessage(a, "gen_ai.completion", 0)); len(m) > 0 {
		return m
	}
	return kit.Messages(openInferenceMessage(a, "llm.output_messages", 0))
}

// systemInstructions is gen_ai.system_instructions: a string, or a JSON list of parts.
func systemInstructions(v any) rudderstackai.GenAIMessage {
	list := v
	if s, ok := v.(string); ok {
		switch j := parsedJSON(s).(type) {
		case map[string]any, []any:
			list = j
		}
	}
	if l, ok := list.([]any); ok {
		return kit.Message("system", flatParts(l)...)
	}
	return kit.Message("system", genAIPart(list)...)
}

// spanCallContent is what a model-call span says went in and what the model asked for.
func spanCallContent(a map[string]attribute.Value) ([]rudderstackai.GenAIMessage, []rudderstackai.GenAIToolCall) {
	input := inputOf(a)
	system := systemInstructions(rawAttr(a, "gen_ai.system_instructions"))
	// A system message already leading the input is the same prompt; do not repeat it.
	if len(system.Parts) > 0 && (len(input) == 0 || input[0].Role != "system") {
		input = append([]rudderstackai.GenAIMessage{system}, input...)
	}
	var calls []rudderstackai.GenAIToolCall
	for _, m := range outputOf(a) {
		for _, p := range m.Parts {
			if name, ok := p["name"].(string); p["type"] == "tool_call" && ok {
				calls = append(calls, rudderstackai.GenAIToolCall{Name: name, Arguments: p["arguments"]})
			}
		}
	}
	return input, calls
}
