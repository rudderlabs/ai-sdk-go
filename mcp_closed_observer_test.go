package rudderstackai

import (
	"context"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestReview84MCPClosedObserver(t *testing.T) {
	setupAI(t)
	s := workspaceServer("closed-observer", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	responses.Close()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	v := &review84Count{}
	rc.ToolDone(mcpbridge.Outcome{Result: v})
	t.Logf("serializer calls=%d; events=%d; no response was written", v.Calls, len(r.named("mcp_tool_called")))
	// A closed observer sees no write: the result is captured as without an observer.
	if v.Calls != 1 || r.props(t, "mcp_tool_called")["tool_status"] != "success" {
		t.Error("closed observer changed the base outcome")
	}
}
