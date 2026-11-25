package kit

import (
	"encoding/json"
	"strings"
	"unicode"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// ToolOutput says how a wrapper's tool result holds the original tool output.
type ToolOutput int

const (
	// RawOutput: a string result is the original output text.
	RawOutput ToolOutput = iota
	// DecodedOutput: the result was decoded from the original output, so a string is a JSON
	// string value and is never decoded again.
	DecodedOutput
)

// Bounds the convention's own decode, in UTF-16 code units as Node.js and Python measure it.
const toolOutputParseLimit = 65536

// ToolOutputFailed applies the one vendor output convention wrappers read
// (spec/fixtures/tool-output-failure-v0.1.json): a plain object with top-level boolean
// successful false. A raw string is decoded for this check only, when short and object-like.
func ToolOutputFailed(v any, output ToolOutput) bool {
	if s, ok := v.(string); ok {
		if output == DecodedOutput || tooLong(s) {
			return false
		}
		trimmed := strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "successful") {
			return false
		}
		if json.Unmarshal([]byte(trimmed), &v) != nil {
			return false
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	successful, ok := m["successful"].(bool)
	return ok && !successful
}

func tooLong(s string) bool {
	// UTF-16 never needs more code units than UTF-8 needs bytes.
	if len(s) <= toolOutputParseLimit {
		return false
	}
	units := 0
	for _, r := range s {
		units++
		if r > 0xFFFF {
			units++
		}
		if units > toolOutputParseLimit {
			return true
		}
	}
	return false
}

// RecordToolOutput records a tool result a wrapper read from a request, failing it as api_error
// when the output follows the vendor convention and no framework failure is already set.
func RecordToolOutput(i *rudderstackai.Interaction, id string, result rudderstackai.ToolResult, hasResult bool, output ToolOutput, name ...string) {
	failed := (result.Error == nil || !*result.Error) && ToolOutputFailed(result.Result, output)
	if failed {
		result.Error, result.ErrorType = rudderstackai.Ptr(true), "api_error"
	}
	hook.RecordToolResult(i, id, result, hasResult, failed, name...)
}
