package rudderbedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"

	ai "github.com/rudderlabs/ai-sdk-go"
)

// unhashableDoc is a document whose type is not comparable; comparableDoc holds one in an
// interface field, so its type is comparable but hashing it panics.
type unhashableDoc struct {
	document.Interface
	tags []string
}

type comparableDoc struct{ document.Interface }

// The helper sends a Converse request whose history carries a json tool result for "t".
func round2Call(t *testing.T, result document.Interface) (*recorder, []any) {
	t.Helper()
	r := setup(t)
	var ruleInputs []any
	if err := ai.Configure(ai.WithEmitSpans(true), ai.WithCaptureContent(true), ai.WithLLMCallEvents(true), ai.WithEventRules(ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": {Event: "looked_up", Properties: func(_ map[string]any, v any) map[string]any {
		ruleInputs = append(ruleInputs, v)
		if m, ok := v.(map[string]any); ok {
			m["added"] = "PRIVATE_ADDED"
		}
		return nil
	}}}})); err != nil {
		t.Fatal(err)
	}
	ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
	owner.RecordToolCalls(ai.ToolCall{ID: "t", Name: "lookup"})
	messages := []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
		ToolUseId: aws.String("t"), Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: result}},
	}}}}}
	_, _, err := (&tracker{cfg: &config{}}).handle(ctx, middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: messages}}, func(context.Context) (middleware.InitializeOutput, middleware.Metadata, error) {
		return middleware.InitializeOutput{}, middleware.Metadata{}, fmt.Errorf("provider error")
	})
	if err == nil {
		t.Fatal("provider error lost")
	}
	owner.End(ai.EndOptions{})
	return r, ruleInputs
}

// Rules mutate their own decoded document without changing the captured call input.
func TestRound2BedrockRuleMutationStaysOutOfInput(t *testing.T) {
	r, inputs := round2Call(t, document.NewLazyDocument(map[string]any{"rows": 1}))
	calls := r.named(t, "ai_llm_response_received")
	wire, _ := json.Marshal(calls)
	if len(inputs) != 1 || len(calls) != 1 || !strings.Contains(string(wire), `"rows"`) || strings.Contains(string(wire), "PRIVATE_ADDED") {
		t.Fatalf("rule inputs=%v; captured %s", inputs, wire)
	}
}

// Documents with unhashable values bypass the cache while preserving tool results and call input.
func TestRound2BedrockUnhashableDocument(t *testing.T) {
	doc := comparableDoc{unhashableDoc{Interface: document.NewLazyDocument(map[string]any{"rows": 1}), tags: []string{"x"}}}
	r, inputs := round2Call(t, doc)
	calls := r.named(t, "ai_llm_response_received")
	wire, _ := json.Marshal(calls)
	if len(inputs) != 1 || fmt.Sprint(inputs[0]) == "<nil>" || !strings.Contains(string(wire), `"rows"`) {
		t.Fatalf("rule inputs=%v; captured %s", inputs, wire)
	}
}
