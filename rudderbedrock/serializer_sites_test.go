package rudderbedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func TestFinalReviewBedrockCallbackShapes(t *testing.T) {
	for _, typedResult := range []bool{false, true} {
		t.Run(fmt.Sprint(typedResult), func(t *testing.T) {
			rec := setup(t)
			var argType, resultType string
			if err := ai.Configure(ai.WithEventRules(ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": {Event: "looked_up", When: func(args map[string]any, result any) bool {
				argType, resultType = fmt.Sprintf("%T", args["quantity"]), fmt.Sprintf("%T", result)
				return args["quantity"].(float64) == 3 && result.(map[string]any)["ok"] == true
			}}}})); err != nil {
				t.Fatal(err)
			}
			llm.ScriptBedrock("bedrock.converse",
				testkit.BedrockReply{RequestID: "req-1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu_1","name":"lookup","input":{"quantity":3}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1}}`)},
				testkit.BedrockReply{RequestID: "req-2", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)},
			)
			ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			c := client()
			first, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("lookup")}})
			if err != nil {
				t.Fatal(err)
			}
			var result any = map[string]any{"ok": true}
			if typedResult {
				result = struct {
					OK bool `json:"ok" document:"ok"`
				}{true}
			}
			second, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("lookup"), first.Output.(*types.ConverseOutputMemberMessage).Value, {Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("tu_1"), Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(result)}}}}}}}})
			if err != nil || second == nil {
				t.Fatalf("provider result=%v error=%v", second, err)
			}
			i.End(ai.EndOptions{})
			t.Logf("args.quantity=%s result=%s declared events=%d", argType, resultType, len(rec.named(t, "looked_up")))
			if len(rec.named(t, "looked_up")) != 1 {
				t.Error("existing callback lost declared tool event")
			}
		})
	}
}

type finalReviewBedrockSerializer struct{ Calls int }

func (v *finalReviewBedrockSerializer) MarshalJSON() ([]byte, error) {
	v.Calls++
	return json.Marshal(map[string]any{"public": fmt.Sprintf("capture-%d", v.Calls)})
}

func TestFinalReviewBedrockToolSerializerCount(t *testing.T) {
	rec := setup(t)
	if err := ai.Configure(ai.WithCaptureContent(true), ai.WithEmitSpans(true), ai.WithEventRules(ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": {Event: "looked_up"}}})); err != nil {
		t.Fatal(err)
	}
	llm.ScriptBedrock("bedrock.converse",
		testkit.BedrockReply{RequestID: "req-1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu_1","name":"lookup","input":{}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1}}`)},
		testkit.BedrockReply{RequestID: "req-2", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)},
	)
	ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
	c := client()
	first, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("lookup")}})
	if err != nil {
		t.Fatal(err)
	}
	v := &finalReviewBedrockSerializer{}
	_, err = c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("lookup"), first.Output.(*types.ConverseOutputMemberMessage).Value, {Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("tu_1"), Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(v)}}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	i.End(ai.EndOptions{})
	b, _ := json.Marshal(rec.named(t, "ai_llm_response_received"))
	t.Logf("serializer calls=%d; call content=%s", v.Calls, b)
	// Smithy decoding deliberately ignores MarshalJSON, just as base did.
	if v.Calls != 0 {
		t.Error("one app value serialized more than once")
	}
}

func TestFinalReviewBedrockDocumentPrivacy(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, site := range []string{"result", "arguments"} {
			t.Run(fmt.Sprintf("spans=%t/%s", spans, site), func(t *testing.T) {
				setup(t)
				dp := dataplane.Start()
				defer dp.Close()
				sender, err := analytics.NewWithConfig("test-key", analytics.Config{DataPlaneUrl: dp.URL, BatchSize: 1, NoProxySupport: true, Interval: 10 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				if err = ai.Configure(ai.WithAnalytics(sender), ai.WithCaptureContent(true), ai.WithEmitSpans(spans)); err != nil {
					t.Fatal(err)
				}
				value := struct {
					Public string `document:"public"`
					Secret string `document:"-"`
				}{Public: "safe", Secret: "PRIVATE_DOCUMENT_CANARY"}
				d := document.NewLazyDocument(value)
				serialized, err := d.MarshalSmithyDocument()
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("Smithy serializer=%s", serialized)
				var history types.Message
				if site == "result" {
					history = types.Message{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("t"), Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: d}}}}}}
				} else {
					history = types.Message{Role: types.ConversationRoleAssistant, Content: []types.ContentBlock{&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{Name: aws.String("lookup"), ToolUseId: aws.String("t"), Input: d}}}}
				}
				llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{RequestID: "req-1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)})
				ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
				_, err = client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{history}})
				if err != nil {
					t.Fatal(err)
				}
				i.End(ai.EndOptions{})
				ai.Flush(context.Background())
				if err = sender.Close(); err != nil {
					t.Fatal(err)
				}
				sent := string(llm.Requests("bedrock.converse")[0])
				t.Logf("provider received private=%t", strings.Contains(sent, "PRIVATE_DOCUMENT_CANARY"))
				if strings.Contains(sent, "PRIVATE_DOCUMENT_CANARY") {
					t.Fatal("invalid repro: provider received excluded value")
				}
				events := dp.Wait(3, 5*time.Second)
				var calls int
				for _, event := range events {
					if event.Event == "ai_llm_response_received" {
						calls++
					}
					wire, _ := json.Marshal(event)
					t.Logf("dataplane %s: private=%t", event.Event, strings.Contains(string(wire), "PRIVATE_DOCUMENT_CANARY"))
					if strings.Contains(string(wire), "PRIVATE_DOCUMENT_CANARY") {
						t.Error("value excluded from transport document leaked to analytics")
					}
				}
				if calls != 1 {
					t.Fatalf("expected call event, got %d", calls)
				}
			})
		}
	}
}

type countingDocument struct {
	document.Interface
	Calls *int
}

func (d *countingDocument) MarshalSmithyDocument() ([]byte, error) {
	*d.Calls++
	return d.Interface.MarshalSmithyDocument()
}
func TestSerializerDocumentPass(t *testing.T) {
	setup(t)
	calls := 0
	d := &countingDocument{Interface: document.NewLazyDocument(map[string]any{"public": "safe"}), Calls: &calls}
	content := []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: d}}
	pass := documentPass{}
	got := toolResultValue(content, pass)
	callback := toolOutcomeValue(content, pass)
	if calls != 1 {
		t.Fatalf("document calls=%d", calls)
	}
	if callback.(map[string]any)["public"] != "safe" || got.(map[string]any)["public"] != "safe" {
		t.Fatal("lost base document shape")
	}
}
