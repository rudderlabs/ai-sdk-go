package rudderanthropic_test

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestToolOutputFailure(t *testing.T) {
	for _, c := range []struct {
		name      string
		content   any
		isError   bool
		status    string
		errorType any
	}{
		{"string", `{"successful":false}`, false, "error", "api_error"},
		{"text block", []any{map[string]any{"type": "text", "text": ` {"successful": false}`}}, false, "error", "api_error"},
		{"double-encoded", `"{\"successful\":false}"`, false, "success", nil},
		{"successful true", `{"successful":true}`, false, "success", nil},
		// is_error keeps its own outcome; only a vendor failure sets error_type.
		{"is_error", `{"successful":false}`, true, "error", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			llm, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {Event: "lookup_done"}}}))
			llm.Script("anthropic.messages", jsonReply(t, map[string]any{"id": "msg_1", "model": "claude-haiku-4-5", "content": []any{}, "stop_reason": "end_turn"}))
			c2 := newClient(llm)
			err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, i *rudderstackai.Interaction) error {
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "toolu_1", Name: "lookup"})
				next := request(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": c.isError, "content": c.content},
				}}}})
				_, err := c2.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, next))
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			ev := rec.events(t)
			if len(ev["lookup_done"]) != 1 {
				t.Fatalf("lookup_done: %d", len(ev["lookup_done"]))
			}
			eq(t, ev["lookup_done"][0], "tool_status", c.status)
			eq(t, ev["lookup_done"][0], "error_type", c.errorType)
			eq(t, ev["lookup_done"][0], "tool_call_id", "toolu_1")
			errors := 0
			if c.status == "error" {
				errors = 1
			}
			eq(t, ev["ai_response_received"][0]["tool_breakdown"].([]any)[0].(map[string]any), "error_count", errors)
		})
	}
}
