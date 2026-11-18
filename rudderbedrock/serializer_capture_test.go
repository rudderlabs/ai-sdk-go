package rudderbedrock

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
)

// The root driver owns the complete ID inventory; this adapter executes its Bedrock cases.
func TestSerializerCapture(t *testing.T) {
	b, err := os.ReadFile("../spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []serializerfixture.Case `json:"cases"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if !c.AppliesToGo() {
			continue
		}
		c, err := c.ResolveGo()
		if err != nil {
			t.Fatal(err)
		}
		if c.Setup["scenario"] != "bedrock_document" && c.Setup["scenario"] != "bedrock_callback_shapes" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			rec := setup(t)
			if err := ai.Configure(ai.WithCaptureContent(true), ai.WithEmitSpans(c.Config.EmitSpans)); err != nil {
				t.Fatal(err)
			}
			if c.Setup["scenario"] == "bedrock_document" {
				fields := c.Setup["fields"].(map[string]any)
				v := struct {
					Public string `document:"public"`
					Secret string `document:"-"`
				}{fields["Public"].(string), fields["Secret"].(string)}
				var native any = v
				if c.Setup["site"] == "pointer_result" {
					native = &v
				}
				if c.Setup["site"] == "nested_result" {
					native = struct {
						Child any `document:"child"`
					}{&v}
				}
				d := document.NewLazyDocument(native)
				wire, err := d.MarshalSmithyDocument()
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(wire), "PRIVATE_DOCUMENT_CANARY") {
					t.Fatal("provider document leaked hidden field")
				}
				pass := documentPass{}
				var captured any
				if c.Setup["site"] == "arguments" {
					parts, _ := blockParts(&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("t"), Name: aws.String("lookup"), Input: d}}, pass)
					captured = parts[0]["arguments"]
				} else {
					content := []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: d}}
					if c.Setup["site"] == "multi_block_result" {
						content = append(content, &types.ToolResultContentBlockMemberText{Value: "visible"})
					}
					messages := []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("t"), Content: content}}}}}
					projected := inputMessages(nil, messages, pass)
					captured = projected[0].Parts[0]["response"]
					if c.Setup["site"] == "multi_block_result" {
						captured = captured.([]ai.GenAIPart)[0]["content"]
					}
				}
				if c.Setup["site"] == "nested_result" {
					captured = captured.(map[string]any)["child"]
				}
				got, _ := json.Marshal(captured)
				want, _ := json.Marshal(c.Expect["decoded"].(map[string]any)["captured_value"])
				if string(got) != string(want) {
					t.Fatalf("captured=%s want=%s", got, want)
				}
			} else {
				args := docValue(document.NewLazyDocument(c.Setup["arguments"])).(map[string]any)
				native := c.Setup["result"]
				switch c.Setup["result_shape"] {
				case "struct":
					native = struct {
						OK bool `document:"ok"`
					}{true}
				case "pointer":
					native = &struct {
						OK bool `document:"ok"`
					}{true}
				case "typed_container":
					native = map[string]bool{"ok": true}
				}
				result := docValue(document.NewLazyDocument(native))
				seen := false
				inspect := func(a map[string]any, r any) {
					seen = true
					if a["quantity"].(float64) != 3 || r.(map[string]any)["ok"] != true {
						t.Fatal("callback shape changed")
					}
				}
				rule := ai.ToolEventRule{Event: "looked_up"}
				if c.Setup["consumer"] == "when" {
					rule.When = func(a map[string]any, r any) bool { inspect(a, r); return true }
				} else {
					rule.Properties = func(a map[string]any, r any) map[string]any {
						inspect(a, r)
						return map[string]any{"ok": r.(map[string]any)["ok"]}
					}
				}
				if err := ai.Configure(ai.WithEventRules(ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": rule}})); err != nil {
					t.Fatal(err)
				}
				_, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
				i.RecordToolCalls(ai.ToolCall{ID: "t", Name: "lookup", Args: args})
				i.RecordToolResult("t", ai.ToolResult{Result: result, Error: ai.Ptr(false)})
				i.End(ai.EndOptions{})
				if !seen || len(rec.named(t, "looked_up")) != 1 {
					t.Fatal("missing declared event")
				}
			}
		})
	}
}
