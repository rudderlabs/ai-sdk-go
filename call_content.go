package rudderstackai

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// Per-call content of ai_llm_response_received (SDK-6798): input messages, output text and the
// tool calls the model requested, in the OpenTelemetry GenAI shapes. Opt-in: WithLLMCallEvents
// and content capture together. Identical in Node.js and Python.

// GenAIPart is one part of a [GenAIMessage]: {"type": "text", "content": "..."}, a "tool_call"
// (id, name, arguments), a "tool_call_response" (id, response), a "uri" (modality, uri,
// mime_type), a "blob" (modality, mime_type; never the bytes) or any other type name.
type GenAIPart = map[string]any

// GenAIMessage is one message in the OpenTelemetry GenAI gen_ai.input.messages shape.
type GenAIMessage struct {
	// Role is system, user, assistant or tool.
	Role  string      `json:"role"`
	Parts []GenAIPart `json:"parts"`
}

// GenAIToolCall is a tool call the model requested. Arguments is any JSON value; a string that
// is valid JSON is parsed. Nil sends no arguments.
type GenAIToolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// callContent is masked per-call content, ready for the event. Values are generic JSON.
type callContent struct {
	inputMessages []any
	// inputTruncated: strings were shortened before masking; fitProperties adds more.
	inputTruncated  bool
	outputText      string
	hasOutputText   bool
	outputToolCalls []any
}

// parseToolArguments parses a JSON-encoded string; anything else is returned as is.
func parseToolArguments(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var out any
	if json.Unmarshal([]byte(s), &out) != nil {
		return v
	}
	return out
}

// cutCodePoints cuts s to at most limit code points.
func cutCodePoints(s string, limit int) string {
	if len(s) <= limit { // a string of max bytes has at most limit code points
		return s
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}

// capStrings shortens every string longer than the masking cap, so masking never withholds a
// long message; hit reports a cut.
func capStrings(v any, hit *bool) any {
	switch x := v.(type) {
	case string:
		c := cutCodePoints(x, maxMaskedCodePoints)
		if len(c) != len(x) {
			*hit = true
		}
		return c
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			out[n] = capStrings(e, hit)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = capStrings(e, hit)
		}
		return out
	}
	return v
}

// maskValue sends every nested string through the redaction path (built-ins, then the hook). A
// withheld string drops the whole value: ok is false.
func maskValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return redactText(x)
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			m, ok := maskValue(e)
			if !ok {
				return nil, false
			}
			out[n] = m
		}
		return out, true
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			m, ok := maskValue(e)
			if !ok {
				return nil, false
			}
			out[k] = m
		}
		return out, true
	}
	return v, true
}

// inlineFree makes a part carry no inline bytes: a blob loses its content, and a data: URI
// (any case) becomes a blob with the part's mime_type, else the URI's media type.
func inlineFree(part map[string]any) map[string]any {
	switch part["type"] {
	case "blob":
		delete(part, "content")
	case "uri":
		uri, _ := part["uri"].(string)
		if len(uri) >= 5 && strings.EqualFold(uri[:5], "data:") {
			delete(part, "uri")
			part["type"] = "blob"
			if m, _ := part["mime_type"].(string); m == "" {
				delete(part, "mime_type")
				if m := dataURIMime(uri); m != "" {
					part["mime_type"] = m
				}
			}
		}
	}
	return part
}

// dataURIMime is the media type of a data: URI (data:image/png;base64,...), when it names one.
func dataURIMime(uri string) string {
	rest := uri[5:]
	if end := strings.IndexAny(rest, ";,"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// genAIMessagesValue is the messages as generic JSON: parts without a string type are skipped, and
// a value that cannot be encoded drops its part.
func genAIMessagesValue(messages []GenAIMessage) []any {
	var out []any
	for _, m := range messages {
		if m.Role == "" {
			continue
		}
		parts := []any{}
		for _, p := range m.Parts {
			if t, _ := p["type"].(string); t == "" {
				continue
			}
			if c, ok := jsonSafe(map[string]any(p)); ok {
				parts = append(parts, inlineFree(c.(map[string]any)))
			}
		}
		out = append(out, map[string]any{"role": m.Role, "parts": parts})
	}
	return out
}

// captureCallContent is the call's content as it will be sent: strings past the masking cap
// shortened, then every string masked like prompt_text. A field with a withheld string (hook
// failure) is left out whole. Nil when nothing is left.
func captureCallContent(inputMessages []GenAIMessage, outputText string, outputToolCalls []GenAIToolCall) *callContent {
	content := &callContent{}
	has := false
	if messages := genAIMessagesValue(inputMessages); len(messages) > 0 {
		hit := false
		if masked, ok := maskValue(capStrings(messages, &hit)); ok {
			content.inputMessages = masked.([]any)
			content.inputTruncated = hit
			has = true
		}
	}
	if outputText != "" {
		if text, ok := redactText(cutCodePoints(outputText, maxMaskedCodePoints)); ok {
			content.outputText, content.hasOutputText = text, true
			has = true
		}
	}
	var calls []any
	for _, t := range outputToolCalls {
		if t.Name == "" {
			continue
		}
		call := map[string]any{"name": t.Name}
		if t.Arguments != nil {
			args, ok := jsonSafe(parseToolArguments(t.Arguments))
			if !ok {
				continue
			}
			call["arguments"] = args
		}
		calls = append(calls, call)
	}
	if len(calls) > 0 {
		hit := false
		if masked, ok := maskValue(capStrings(calls, &hit)); ok {
			content.outputToolCalls = masked.([]any)
			has = true
		}
	}
	if !has {
		return nil
	}
	return content
}

// leaf is a string a trim may shorten, and where it lives.
type leaf struct {
	holder any // map[string]any or []any
	key    any // string or int
	length int
	system bool
}

// contentKeys name the strings that count as content: a part's content, response and arguments,
// at any depth.
var contentKeys = map[string]bool{"content": true, "response": true, "arguments": true}

func collectLeaves(v any, out *[]leaf, system, inContent bool) {
	visit := func(holder, key, e any) {
		k, isKey := key.(string)
		content := inContent || (isKey && contentKeys[k])
		switch x := e.(type) {
		case string:
			if content {
				*out = append(*out, leaf{holder, key, utf8.RuneCountInString(x), system})
			}
		case []any, map[string]any:
			collectLeaves(x, out, system, content)
		}
	}
	switch x := v.(type) {
	case []any:
		for n, e := range x {
			visit(x, n, e)
		}
	case map[string]any:
		// Keys in the order Go encodes them, so document order is the emitted order.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(x, k, x[k])
		}
	}
}

func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			out[n] = deepCopyJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopyJSON(e)
		}
		return out
	}
	return v
}

// jsonBytes is the UTF-8 bytes one code point takes inside a compact JSON string (escapes counted).
func jsonBytes(r rune) int { return byteSize(string(r)) - 2 }

// trimInputMessages returns input messages within over bytes of the budget (the event's compact
// JSON, in UTF-8 bytes). First the oldest messages after the leading system ones go, one at a
// time, each saving its compact JSON bytes + 1; the last message always stays. Then the longest
// content string loses its tail, only as many code points as their encoded bytes need to cover
// over; ties go to the first in message order, and strings of system messages are shortened only
// when no other string has anything left to cut. ok is false when every string is empty and it
// still does not fit.
func trimInputMessages(messages []any, over int) ([]any, bool) {
	kept := append([]any(nil), messages...)
	first := len(kept)
	for n, m := range kept {
		if role, _ := m.(map[string]any)["role"].(string); role != "system" {
			first = n
			break
		}
	}
	for over > 0 && len(kept) > first+1 {
		dropped := kept[first]
		kept = append(kept[:first], kept[first+1:]...)
		over -= byteSize(dropped) + 1
	}
	if over <= 0 {
		return kept, true
	}
	copied := deepCopyJSON(kept).([]any)
	for over > 0 {
		var all []leaf
		for _, m := range copied {
			role, _ := m.(map[string]any)["role"].(string)
			collectLeaves(m, &all, role == "system", false)
		}
		longest := func(system bool) *leaf {
			var best *leaf
			for n := range all {
				if all[n].system == system && all[n].length > 0 && (best == nil || all[n].length > best.length) {
					best = &all[n]
				}
			}
			return best
		}
		target := longest(false)
		if target == nil {
			target = longest(true)
		}
		if target == nil {
			return nil, false
		}
		var text string
		switch h := target.holder.(type) {
		case map[string]any:
			text = h[target.key.(string)].(string)
		case []any:
			text = h[target.key.(int)].(string)
		}
		runes := []rune(text)
		for len(runes) > 0 && over > 0 {
			over -= jsonBytes(runes[len(runes)-1])
			runes = runes[:len(runes)-1]
		}
		switch h := target.holder.(type) {
		case map[string]any:
			h[target.key.(string)] = string(runes)
		case []any:
			h[target.key.(int)] = string(runes)
		}
	}
	return copied, true
}
