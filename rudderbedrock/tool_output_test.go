package rudderbedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestToolOutputFailure(t *testing.T) {
	text := func(s string) types.ToolResultContentBlock { return &types.ToolResultContentBlockMemberText{Value: s} }
	jsonBlock := func(v any) types.ToolResultContentBlock {
		return &types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(v)}
	}
	for _, c := range []struct {
		name      string
		block     types.ToolResultContentBlock
		status    types.ToolResultStatus
		want      string
		errorType any
	}{
		{"json object", jsonBlock(map[string]any{"successful": false}), "", "error", "api_error"},
		{"text", text(` {"successful":false}`), "", "error", "api_error"},
		// A json block's string is a decoded value, never decoded again.
		{"json string", jsonBlock(`{"successful":false}`), "", "success", nil},
		{"double-encoded text", text(`"{\"successful\":false}"`), "", "success", nil},
		{"status error keeps its outcome", jsonBlock(map[string]any{"successful": false}), types.ToolResultStatusError, "error", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := setup(t)
			if err := rudderstackai.Configure(rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {Event: "lookup_done"}}})); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithEventRules(rudderstackai.EventRules{})) })
			llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{RequestID: "req-1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)})
			err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, i *rudderstackai.Interaction) error {
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "tu_1", Name: "lookup"})
				results := types.Message{Role: types.ConversationRoleUser, Content: []types.ContentBlock{
					&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("tu_1"), Status: c.status, Content: []types.ToolResultContentBlock{c.block}}},
				}}
				_, err := client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi"), results}})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			events := rec.named(t, "lookup_done")
			if len(events) != 1 {
				t.Fatalf("lookup_done: %d", len(events))
			}
			check(t, events[0], map[string]any{"tool_status": c.want, "error_type": c.errorType, "tool_call_id": "tu_1"})
			errors := 0
			if c.want == "error" {
				errors = 1
			}
			check(t, rec.named(t, "ai_response_received")[0]["tool_breakdown"].([]any)[0].(map[string]any), map[string]any{"error_count": errors})
		})
	}
}
