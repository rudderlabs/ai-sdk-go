package rudderopenai

import (
	"strings"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

type part = rudderstackai.GenAIPart

// openaiParts maps a message's or Responses item's content: a string, or typed parts.
func openaiParts(content any) []part {
	if s, ok := content.(string); ok {
		return kit.TextPart(s)
	}
	var out []part
	for _, c := range kit.List(content) {
		p := kit.Map(c)
		typ, _ := p["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			out = append(out, kit.TextPart(p["text"])...)
		case "refusal":
			out = append(out, kit.TextPart(p["refusal"])...)
		case "image_url":
			out = append(out, kit.MediaPart("image", kit.Map(p["image_url"])["url"])...)
		case "input_image":
			out = append(out, kit.MediaPart("image", p["image_url"])...)
		case "input_audio":
			out = append(out, kit.BlobPart("audio", kit.Map(p["input_audio"])["format"]))
		default:
			out = append(out, kit.OtherPart(typ)...)
		}
	}
	return out
}

// textOrParts is a tool result's content: text as a string, anything else as its parts.
func textOrParts(content any) any {
	if content == nil {
		return nil
	}
	if _, ok := content.(string); ok {
		return content
	}
	parts := openaiParts(content)
	var b strings.Builder
	for _, p := range parts {
		if p["type"] != "text" {
			return parts
		}
		b.WriteString(kit.Str(p["content"]))
	}
	return b.String()
}

func roleOf(role any) string {
	if role == "developer" {
		return "system"
	}
	s, _ := role.(string)
	return s
}

// chatInputMessages maps Chat Completions messages (developer is system).
func chatInputMessages(req map[string]any) []rudderstackai.GenAIMessage {
	var out []rudderstackai.GenAIMessage
	for _, item := range kit.List(req["messages"]) {
		m := kit.Map(item)
		if m == nil {
			continue
		}
		if m["role"] == "tool" || m["role"] == "function" {
			id := m["tool_call_id"]
			if id == nil {
				id = m["name"]
			}
			out = append(out, kit.Message("tool", kit.ToolResponsePart(id, textOrParts(m["content"]))))
			continue
		}
		parts := openaiParts(m["content"])
		for _, t := range kit.List(m["tool_calls"]) {
			tc := kit.Map(t)
			parts = append(parts, kit.ToolCallPart(tc["id"], chatToolName(tc), chatToolArgs(tc))...)
		}
		if legacy := kit.Map(m["function_call"]); legacy != nil {
			parts = append(parts, kit.ToolCallPart(nil, legacy["name"], legacy["arguments"])...)
		}
		out = append(out, kit.Message(roleOf(m["role"]), parts...))
	}
	return kit.Messages(out...)
}

// responsesInputMessages maps Responses instructions and input.
func responsesInputMessages(req map[string]any) []rudderstackai.GenAIMessage {
	var list []rudderstackai.GenAIMessage
	if s, ok := req["instructions"].(string); ok {
		list = append(list, kit.Message("system", kit.TextPart(s)...))
	}
	if s, ok := req["input"].(string); ok {
		list = append(list, kit.Message("user", kit.TextPart(s)...))
	}
	for _, it := range kit.List(req["input"]) {
		item := kit.Map(it)
		if item == nil {
			continue
		}
		typ, ok := item["type"].(string)
		if !ok {
			typ = "message"
		}
		switch {
		case typ == "function_call" || typ == "custom_tool_call":
			id := item["call_id"]
			if id == nil {
				id = item["id"]
			}
			args := item["arguments"]
			if args == nil {
				args = item["input"]
			}
			list = append(list, kit.Message("assistant", kit.ToolCallPart(id, item["name"], args)...))
		case strings.HasSuffix(typ, "_call_output"):
			list = append(list, kit.Message("tool", kit.ToolResponsePart(item["call_id"], textOrParts(item["output"]))))
		case typ == "message":
			list = append(list, kit.Message(roleOf(item["role"]), openaiParts(item["content"])...))
		default:
			list = append(list, kit.Message("assistant", kit.OtherPart(typ)...))
		}
	}
	return kit.Messages(list...)
}
