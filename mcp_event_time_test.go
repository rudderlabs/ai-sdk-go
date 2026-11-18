package rudderstackai

import (
	"context"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestMCPEventTimeIsHandlerCompletion(t *testing.T) {
	setupAI(t)
	s := workspaceServer("event-time-mcp", nil)
	rec := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "looked_up"}}}})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	defer responses.Close()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	done := time.Now()
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"structuredContent": map[string]any{"ok": true}}})
	time.Sleep(60 * time.Millisecond)
	responses.DeliverJSON([]byte(`{"id":1,"result":{"structuredContent":{"ok":true}}}`))
	for _, event := range []string{"mcp_tool_called", "looked_up"} {
		at := rec.wait(t, event, 1)[0].OriginalTimestamp
		if late := at.Sub(done); late > 40*time.Millisecond {
			t.Errorf("%s stamped %s after the handler finished", event, late)
		}
	}
}
