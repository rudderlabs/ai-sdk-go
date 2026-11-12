package kit

import (
	"regexp"
	"strings"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Builders for GenAI input messages (input_messages), shared by every integration so the same
// request maps to the same parts. Nothing is masked or capped here: the SDK does that.

// Message is {role, parts}; a message without parts is left out by Messages.
func Message(role string, parts ...rudderstackai.GenAIPart) rudderstackai.GenAIMessage {
	return rudderstackai.GenAIMessage{Role: role, Parts: parts}
}

// Messages keeps the messages that have at least one part, in order.
func Messages(list ...rudderstackai.GenAIMessage) []rudderstackai.GenAIMessage {
	var out []rudderstackai.GenAIMessage
	for _, m := range list {
		if len(m.Parts) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// Parts joins part lists.
func Parts(lists ...[]rudderstackai.GenAIPart) []rudderstackai.GenAIPart {
	var out []rudderstackai.GenAIPart
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// TextPart is a text part; empty and non-string text gives none.
func TextPart(content any) []rudderstackai.GenAIPart {
	if s, ok := content.(string); ok && s != "" {
		return []rudderstackai.GenAIPart{{"type": "text", "content": s}}
	}
	return nil
}

// ReasoningPart is a reasoning or thinking part.
func ReasoningPart(content any) []rudderstackai.GenAIPart {
	if s, ok := content.(string); ok && s != "" {
		return []rudderstackai.GenAIPart{{"type": "reasoning", "content": s}}
	}
	return nil
}

// ToolCallPart is a tool call the model made earlier; JSON-string arguments are parsed.
func ToolCallPart(id, name, args any) []rudderstackai.GenAIPart {
	n, ok := name.(string)
	if !ok || n == "" {
		return nil
	}
	p := rudderstackai.GenAIPart{"type": "tool_call", "name": n}
	if s, ok := id.(string); ok && s != "" {
		p["id"] = s
	}
	if args != nil {
		p["arguments"] = ParseToolValue(args)
	}
	return []rudderstackai.GenAIPart{p}
}

// ToolResponsePart is a tool result sent back to the model; response is kept as the provider sent it.
func ToolResponsePart(id, response any) rudderstackai.GenAIPart {
	p := rudderstackai.GenAIPart{"type": "tool_call_response"}
	if s, ok := id.(string); ok && s != "" {
		p["id"] = s
	}
	if response != nil {
		p["response"] = response
	}
	return p
}

// URIPart is a file or image given by URL; a data: URI is inline data, so it becomes a blob
// without its bytes.
func URIPart(modality string, uri, mimeType any) []rudderstackai.GenAIPart {
	u, ok := uri.(string)
	if !ok || u == "" {
		return nil
	}
	if len(u) >= 5 && strings.EqualFold(u[:5], "data:") {
		m, _ := mimeType.(string)
		if m == "" {
			if match := dataURI.FindStringSubmatch(u); match != nil {
				m = match[1]
			}
		}
		return []rudderstackai.GenAIPart{BlobPart(modality, m)}
	}
	p := rudderstackai.GenAIPart{"type": "uri", "modality": modality, "uri": u}
	if m, ok := mimeType.(string); ok && m != "" {
		p["mime_type"] = m
	}
	return []rudderstackai.GenAIPart{p}
}

// BlobPart is inline binary (base64 images, audio, files): its kind only, never the bytes.
func BlobPart(modality string, mimeType any) rudderstackai.GenAIPart {
	p := rudderstackai.GenAIPart{"type": "blob", "modality": modality}
	if m, ok := mimeType.(string); ok && m != "" {
		p["mime_type"] = m
	}
	return p
}

// OtherPart is a part the SDK does not map: its type name only.
func OtherPart(typ any) []rudderstackai.GenAIPart {
	if s, ok := typ.(string); ok && s != "" {
		return []rudderstackai.GenAIPart{{"type": s}}
	}
	return nil
}

var dataURI = regexp.MustCompile(`(?i)^data:([^;,]*)`)

// MediaPart is a URL or data: URI as a part: inline data keeps its kind only (a blob).
func MediaPart(modality string, url any) []rudderstackai.GenAIPart {
	return URIPart(modality, url, nil)
}

// ToolCalls are the tool calls the model requested, as output_tool_calls; JSON-string arguments
// are parsed by the SDK.
func ToolCalls(calls []rudderstackai.ToolCall) []rudderstackai.GenAIToolCall {
	var out []rudderstackai.GenAIToolCall
	for _, t := range calls {
		if t.Name != "" {
			out = append(out, rudderstackai.GenAIToolCall{Name: t.Name, Arguments: t.Args})
		}
	}
	return out
}
