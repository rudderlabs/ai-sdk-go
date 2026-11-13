package rudderstackai

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNativeRequestContextWithoutListing(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "registered_before_wrap"
		if late {
			name = "registered_after_wrap"
		}
		t.Run(name, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: "native-context", Version: "1"}, nil)
			var got string
			add := func() {
				s.AddTool(&mcp.Tool{Name: "own", InputSchema: map[string]any{
					"type": "object", "properties": map[string]any{requestContext: map[string]any{"type": "object"}},
				}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					got = string(req.Params.Arguments)
					return &mcp.CallToolResult{}, nil
				})
			}
			if !late {
				add()
			}
			r := wrap(t, s, Options{CaptureContent: true, RequestContext: RequestContextOptions{Enabled: true, Intents: []string{"debug_delivery"}}})
			if late {
				add()
			}
			cs := connect(t, s, "2025-06-18", nil)
			args := map[string]any{requestContext: map[string]any{
				"intent": " debug_delivery ", "user_sentiment": "negative", "call_reason": "mine", "custom": "untouched",
			}}
			callTool(t, cs, "own", args, nil)
			if got != mustJSON(args) {
				t.Errorf("handler got %s, want %s", got, mustJSON(args))
			}
			p := r.props(t, "mcp_tool_called")
			for _, key := range []string{"intent", "user_sentiment", "call_reason"} {
				if _, ok := p[key]; ok {
					t.Errorf("native context reported as %s: %v", key, p)
				}
			}
		})
	}
}

func TestRequestContextIntentTrimmed(t *testing.T) {
	for _, tc := range []struct {
		name, intent, want string
	}{
		{"matching", " debug_delivery ", "debug_delivery"},
		{"unknown", " unknown ", "other"},
		{"whitespace", " \t\n\u2003 ", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := &handlerSeen{}
			s := workspaceServer("intent-context", seen)
			r := wrap(t, s, Options{RequestContext: RequestContextOptions{Enabled: true, Intents: []string{"debug_delivery"}}})
			cs := connect(t, s, "2025-06-18", nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w", requestContext: map[string]any{"intent": tc.intent}}, nil)
			p := r.props(t, "mcp_tool_called")
			if got, present := p["intent"]; tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("intent = %v (present %t), want %q", got, present, tc.want)
			}
			if seen.last() != `{"workspace_id":"w"}` {
				t.Errorf("handler got %s", seen.last())
			}
		})
	}
}
