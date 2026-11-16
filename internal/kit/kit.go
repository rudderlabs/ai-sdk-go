// Package kit is the shared machinery of the provider integrations: typed access to the
// interaction hooks, HTTP interception of a provider's requests, and stream bookkeeping.
package kit

import (
	"context"
	"encoding/json"
	"errors"
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

// CapturesCallContent reports whether each call's input and output (LLMCall.InputMessages,
// OutputText, OutputToolCalls) should be handed over: call events and content capture are on.
func CapturesCallContent(i *rudderstackai.Interaction) bool { return hook.CapturesCallContent(i) }

// FirstToken marks the first streamed token.
func FirstToken(i *rudderstackai.Interaction, at time.Time) { hook.FirstToken(i, at) }

// HintOutcome sets the status End reports unless a call is recorded afterwards.
func HintOutcome(i *rudderstackai.Interaction, errorType string) {
	hook.HintOutcome(i, hook.StatusForErrorType(errorType), errorType, "")
}

// HintStatus sets the status End reports unless a call is recorded afterwards; pauseReason says
// what a paused run waits for.
func HintStatus(i *rudderstackai.Interaction, status rudderstackai.ResponseStatus, errorType string, pauseReason rudderstackai.PauseReason) {
	hook.HintOutcome(i, string(status), errorType, string(pauseReason))
}

// StartInteraction starts an interaction the integration opens after the fact: tagged with the
// integration, offered its client, and started at startedAt when it is set.
func StartInteraction(ctx context.Context, o rudderstackai.InteractionOptions, integration string, client analytics.Client, startedAt time.Time) (context.Context, *rudderstackai.Interaction) {
	ctx, i := hook.StartInteraction(ctx, o, integration, client, startedAt)
	return ctx, i.(*rudderstackai.Interaction)
}

// Adopt tags an interaction with the integration and offers its client; the first of each wins.
func Adopt(i *rudderstackai.Interaction, integration string, client analytics.Client) {
	hook.Adopt(i, integration, client)
}

// PrimaryModel sets primary_model and primary_provider unless a later call succeeds.
func PrimaryModel(i *rudderstackai.Interaction, model, provider string) {
	hook.PrimaryModel(i, model, provider)
}

// KeepAnswer keeps answer text for the classifier and evaluator.
func KeepAnswer(i *rudderstackai.Interaction, text string) { hook.KeepAnswer(i, text) }

// Backdate moves a resumed segment's start back to when its events happened, taking the time from
// the latest pause.
func Backdate(i *rudderstackai.Interaction, startedAt time.Time) { hook.Backdate(i, startedAt) }

// AppErrorType is the error_type of an error the app's own code ended an answer with: app_error
// for one without a provider signal.
func AppErrorType(err error) string { return hook.AppErrorType(err) }

// StatusForErrorType is the response_status an error type ends an answer with.
func StatusForErrorType(errorType string) rudderstackai.ResponseStatus {
	return rudderstackai.ResponseStatus(hook.StatusForErrorType(errorType))
}

// ContextConversation is the conversation ID a context carries (rudderstackai.WithValues), or "".
func ContextConversation(ctx context.Context) string { return hook.ContextConversation(ctx) }

// WarnOnce logs a warning the first time key is seen.
func WarnOnce(key, message string) { hook.WarnOnce(key, message) }

// MaxCallsPerInteraction is the configured cap of model and tool calls stored per interaction.
func MaxCallsPerInteraction() int { return hook.MaxCallsPerInteraction() }

// MaxMapEntries is the configured cap of each SDK lookup map.
func MaxMapEntries() int { return hook.MaxMapEntries() }

// LimitReached reports that the lookup map where dropped its oldest entries at the cap.
func LimitReached(where string) {
	hook.Report("limit_reached", "MaxMapEntries ("+where+")", errMapFull)
}

var errMapFull = errors.New("rudderstackai: a lookup map reached limits.MaxMapEntries; dropped its oldest entries")

// OnBeforeEnd runs fn just before the interaction's ai_response_received is built.
func OnBeforeEnd(i *rudderstackai.Interaction, fn func()) { hook.OnBeforeEnd(i, fn) }

// Untracked reports whether calls with this context are the SDK's own.
func Untracked(ctx context.Context) bool { return hook.Untracked(ctx) }

// NoteCallError remembers the error a call failed with, so the app returning it keeps the call's
// errorType rather than becoming app_error.
func NoteCallError(i *rudderstackai.Interaction, err error, errorType string) {
	hook.NoteCallError(i, err, errorType)
}

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

// RecordToolResult preserves observed JSON null separately from an omitted tool result.
// The optional name matches only id-less forced Gemini candidates.
func RecordToolResult(i *rudderstackai.Interaction, id string, result rudderstackai.ToolResult, hasResult bool, name ...string) {
	hook.RecordToolResult(i, id, result, hasResult, name...)
}
