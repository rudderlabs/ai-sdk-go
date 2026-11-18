package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

type review84TaggedProperties struct {
	Number int   `json:"number,string"`
	Empty  []int `json:"empty,omitempty"`
}

type review84PublicEmbedded struct {
	Value string `json:"value"`
}

type review84PrivateEmbedded struct {
	Secret string `json:"value"`
}

func review84ConflictProperties() any {
	// Build anonymous conflicting fields at runtime so vet does not reject the intentional conflict.
	t := reflect.StructOf([]reflect.StructField{
		{Name: "Public", Type: reflect.TypeFor[review84PublicEmbedded](), Anonymous: true},
		{Name: "Private", Type: reflect.TypeFor[review84PrivateEmbedded](), Anonymous: true},
	})
	v := reflect.New(t).Elem()
	v.Field(0).Set(reflect.ValueOf(review84PublicEmbedded{"public"}))
	v.Field(1).Set(reflect.ValueOf(review84PrivateEmbedded{"PRIVATE_CONFLICT_CANARY"}))
	return v.Interface()
}

type review84Failure struct{}

func (*review84Failure) MarshalJSON() ([]byte, error) { return nil, fmt.Errorf("fail") }

func TestReview84PropertyConflictPrivacy(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			value := review84ConflictProperties()
			expected, _ := json.Marshal(value)
			rule := ToolEventRule{Event: "looked_up", Properties: func(map[string]any, any) map[string]any { return map[string]any{"custom": value} }}
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": rule}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			got := mustJSON(r.named("looked_up")[0].Properties["custom"])
			t.Logf("encoding/json=%s; custom=%s", expected, got)
			if strings.Contains(got, "PRIVATE_CONFLICT_CANARY") {
				t.Error("JSON-excluded field leaked through tool properties")
			}
		})
	}
}

func TestReview84PropertyEncoding(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			rule := ToolEventRule{Event: "looked_up", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"custom": review84TaggedProperties{Number: 3, Empty: []int{}}}
			}}
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": rule}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
			i.RecordToolResult("t", ToolResult{Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			got := mustJSON(r.named("looked_up")[0].Properties["custom"])
			t.Logf("custom=%s", got)
			if got != `{"number":"3"}` {
				t.Errorf("existing encoding/json properties changed")
			}
		})
	}
}

func TestReview84MCPPropertyConflictPrivacy(t *testing.T) {
	setupAI(t)
	value := review84ConflictProperties()
	s := workspaceServer("property-conflict", nil)
	r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any { return map[string]any{"custom": value} }}}}})
	w, _ := instances.get(s)
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"content": []any{}}})
	got := mustJSON(r.named("looked_up")[0].Properties["custom"])
	t.Logf("MCP custom=%s", got)
	if strings.Contains(got, "PRIVATE_CONFLICT_CANARY") {
		t.Error("JSON-excluded field leaked through MCP properties")
	}
}

func TestReview84RootFailureNull(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithLLMCallEvents(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: &review84Failure{}}}})
			i.End(EndOptions{})
			got := mustJSON(r.named("ai_llm_response_received")[0].Properties["output_tool_calls"])
			t.Logf("top-level failing argument=%s", got)
			if !strings.Contains(got, `"arguments":null`) {
				t.Error("failure placeholder omitted at tool argument root")
			}
		})
	}
}

func TestReview84MCPResultSizeEncoding(t *testing.T) {
	setupAI(t)
	s := workspaceServer("tagged-size", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	v := &mcp.CallToolResult{StructuredContent: review84TaggedProperties{Number: 3, Empty: []int{}}}
	b, _ := json.Marshal(v)
	rc.ToolDone(mcpbridge.Outcome{Result: v})
	p := r.props(t, "mcp_tool_called")
	t.Logf("wire=%s bytes=%d; reported size=%v", b, len(b), p["result_size_bytes"])
	if p["result_size_bytes"] != len(b) {
		t.Error("size differs from base wire encoding")
	}
}

func round1MCPProperties(t *testing.T, rule ToolEventRule) any {
	setupAI(t)
	s := workspaceServer("round1-properties", nil)
	r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": rule}}})
	w, _ := instances.get(s)
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"content": []any{}}})
	return r.props(t, "lookup_done")["custom"]
}
