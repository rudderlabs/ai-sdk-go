// Package kit is the shared machinery of the provider integrations: typed access to the
// interaction hooks, HTTP interception of a provider's requests, and stream bookkeeping.
package kit

import (
	"context"
	"encoding/json"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// ForCall returns the interaction a model call starting now reports into, and whether the
// integration opened it (and must end it). Nil for the SDK's own (untracked) calls.
func ForCall(ctx context.Context, integration string, client analytics.Client) (*rudderstackai.Interaction, bool) {
	i, ephemeral := hook.ForCall(ctx, integration, client)
	if i == nil {
		return nil, false
	}
	return i.(*rudderstackai.Interaction), ephemeral
}

// KeepMessages keeps a request's messages (decoded JSON) for the classifier and evaluator.
func KeepMessages(i *rudderstackai.Interaction, messages any) { hook.KeepMessages(i, messages) }

// KeepsText reports whether answer text should be collected for the classifier or evaluator.
func KeepsText(i *rudderstackai.Interaction) bool { return hook.KeepsText(i) }

// FirstToken marks the first streamed token.
func FirstToken(i *rudderstackai.Interaction, at time.Time) { hook.FirstToken(i, at) }

// HintOutcome sets the status End reports unless a call is recorded afterwards.
func HintOutcome(i *rudderstackai.Interaction, errorType string) {
	hook.HintOutcome(i, hook.StatusForErrorType(errorType), errorType)
}

// OnBeforeEnd runs fn just before the interaction's ai_response_received is built.
func OnBeforeEnd(i *rudderstackai.Interaction, fn func()) { hook.OnBeforeEnd(i, fn) }

// Untracked reports whether calls with this context are the SDK's own.
func Untracked(ctx context.Context) bool { return hook.Untracked(ctx) }

// RegisterErrorClassifier adds an integration's mapping of its SDK's errors to error types.
func RegisterErrorClassifier(fn func(error) (string, bool)) { hook.RegisterErrorClassifier(fn) }

// EndEphemeral ends an interaction the integration opened: with the status of errorType, or
// derived from its calls when errorType is empty.
func EndEphemeral(i *rudderstackai.Interaction, errorType string) {
	if errorType == "" {
		i.End(rudderstackai.EndOptions{})
		return
	}
	i.End(rudderstackai.EndOptions{Status: rudderstackai.ResponseStatus(hook.StatusForErrorType(errorType)), ErrorType: errorType})
}

// ParseToolValue parses a JSON-encoded tool argument or result; anything else is returned as is.
func ParseToolValue(v any) any {
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

// ToolResultValue is an Anthropic tool_result content as rules see it: a string or a single text
// block is parsed like an OpenAI tool output; other content stays as sent.
func ToolResultValue(content any) any {
	if s, ok := content.(string); ok {
		return ParseToolValue(s)
	}
	if list, ok := content.([]any); ok && len(list) == 1 {
		if b, ok := list[0].(map[string]any); ok && b["type"] == "text" {
			if t, ok := b["text"].(string); ok {
				return ParseToolValue(t)
			}
		}
	}
	return content
}

// Int returns a JSON number (float64) as an int, or 0.
func Int(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		return max(n, 0)
	case json.Number:
		f, _ := n.Float64()
		if f > 0 {
			return int(f)
		}
	}
	return 0
}

// IntPtr returns a JSON number as an *int, or nil when absent or not a number.
func IntPtr(v any) *int {
	switch v.(type) {
	case float64, int, json.Number:
		n := Int(v)
		return &n
	}
	return nil
}

// Str returns v when it is a string.
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Map returns v when it is a JSON object.
func Map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// List returns v when it is a JSON array.
func List(v any) []any {
	l, _ := v.([]any)
	return l
}
