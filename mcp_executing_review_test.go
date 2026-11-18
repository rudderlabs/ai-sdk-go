package rudderstackai

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestExecutingMCPWireAndTiming(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("wire", nil)
			r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "done", Properties: func(_ map[string]any, v any) map[string]any { return map[string]any{"visible": v} }}}}})
			w, _ := instances.get(s)
			responses := mcpbridge.NewResponses()
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			v := &fixtureSerializer{Output: map[string]any{"structuredContent": "INTERNAL_PRIVATE"}}
			rc.ToolDone(mcpbridge.Outcome{Result: v})
			time.Sleep(40 * time.Millisecond)
			if failed {
				responses.Deliver(map[string]any{"id": 1, "error": map[string]any{"code": -32602, "message": "CLIENT_ERROR"}})
			} else {
				responses.Deliver(map[string]any{"id": 1, "result": map[string]any{"structuredContent": "CLIENT_VISIBLE"}})
			}
			p := r.props(t, "mcp_tool_called")
			if p["duration_ms"].(int64) != rc.completed.Sub(rc.start).Milliseconds() {
				t.Errorf("handler duration includes write: %v", p["duration_ms"])
			}
			if v.Calls != 0 {
				t.Errorf("SDK serialized provider object %d times", v.Calls)
			}
			wire := mustJSON(r.named("done"))
			if strings.Contains(wire, "INTERNAL_PRIVATE") || (!failed && !strings.Contains(wire, "CLIENT_VISIBLE")) {
				t.Errorf("captured %s", wire)
			}
			if failed && p["tool_status"] != "error" {
				t.Errorf("wire error status=%v", p)
			}
		})
	}
}
func TestExecutingMCPRuleClosure(t *testing.T) {
	setupAI(t)
	v := &fixtureSerializer{Output: "visible"}
	s := workspaceServer("closure", nil)
	r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "done", Properties: func(map[string]any, any) map[string]any { return map[string]any{"custom": v} }}}}})
	w, _ := instances.get(s)
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"structuredContent": v}})
	if v.Calls != 1 || r.props(t, "done")["custom"] != "visible" {
		t.Fatalf("calls=%d props=%v", v.Calls, r.props(t, "done"))
	}
}
