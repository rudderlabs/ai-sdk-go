package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func round2Rule(seen *[]string) EventRules {
	return EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", When: func(args map[string]any, result any) bool {
		*seen = append(*seen, fmt.Sprintf("args=%s result=%s", mustJSON(args), mustJSON(result)))
		return true
	}}}}
}

// A provider tool result whose MarshalJSON uses json.Marshal: result_size_bytes is base's size,
// escapes included, whether the result comes with the call or in RecordToolResult.
func TestRound2ToolResultEscapedSerializerSize(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, report := range []string{"result", "calls"} {
			t.Run(fmt.Sprintf("capture=%v/%s", capture, report), func(t *testing.T) {
				var seen []string
				r := setupAI(t, WithCaptureContent(capture), WithEventRules(round2Rule(&seen)))
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				result := round2EscapedJSON{URL: "https://x.test/?a=1&b=2 <ok>"}
				wire, _ := json.Marshal(result)
				if report == "result" {
					i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
					i.RecordToolResult("t", ToolResult{Result: result, Error: Ptr(false)})
				} else {
					i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: result, Error: Ptr(false)})
				}
				i.End(EndOptions{})
				if p := r.props(t, "looked_up"); p["result_size_bytes"] != len(wire) {
					t.Fatalf("result_size_bytes=%v, base gives the serializer's encoded size %d", p["result_size_bytes"], len(wire))
				}
			})
		}
	}
}

// A follow-up RecordToolCalls with only the ID and result ("its name and outcome fill in")
// measures the result under the stored name's rule, as base did.
func TestRound2NamelessFollowUpResultSize(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, spans := range []bool{false, true} {
			t.Run(fmt.Sprintf("capture=%t/spans=%t", capture, spans), func(t *testing.T) {
				var seen []string
				r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(capture), WithEventRules(round2Rule(&seen)))
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: map[string]any{"q": "x"}})
				i.RecordToolCalls(ToolCall{ID: "t", Result: map[string]any{"rows": []any{1, 2, 3}}, Error: Ptr(false)})
				i.End(EndOptions{})
				if p := r.props(t, "looked_up"); p["result_size_bytes"] != len(`{"rows":[1,2,3]}`) {
					t.Fatalf("result_size_bytes=%v", p["result_size_bytes"])
				}
			})
		}
	}
}

// A tool whose name arrives in a later report keeps its arguments for the rule.
func TestRound2NameFilledLaterArgs(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%t", capture), func(t *testing.T) {
			var seen []string
			r := setupAI(t, WithCaptureContent(capture), WithEventRules(round2Rule(&seen)))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Args: map[string]any{"q": "x"}})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
			i.RecordToolResult("t", ToolResult{Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			if len(seen) != 1 || seen[0] != `args={"q":"x"} result="ok"` || len(r.named("looked_up")) != 1 {
				t.Fatalf("rule inputs changed: %v", seen)
			}
		})
	}
}

// The provider-tool sibling of the MCP Buffer-shaped properties: with spans on, a rule's
// properties map is a container, so its own shape never drops every property.
func TestRound2ToolRuleBufferShapedProperties(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"type": "Buffer", "data": []int{1, 2}}
			}}}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			if p := r.props(t, "looked_up"); p["type"] != "Buffer" || mustJSON(p["data"]) != "[1,2]" {
				t.Fatalf("properties=%v", p)
			}
		})
	}
}
