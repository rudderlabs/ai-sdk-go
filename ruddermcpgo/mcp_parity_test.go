package ruddermcpgo

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestNativeRequestContextWithoutListing(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "registered_before_wrap"
		if late {
			name = "registered_after_wrap"
		}
		t.Run(name, func(t *testing.T) {
			s := server.NewMCPServer("native-context", "1", server.WithToolCapabilities(true))
			var got any
			add := func() {
				s.AddTool(mcp.NewTool("own", mcp.WithObject(requestContext)), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					got = req.GetArguments()
					return mcp.NewToolResultText("ok"), nil
				})
			}
			if !late {
				add()
			}
			r := wrap(t, s, Options{CaptureContent: true, RequestContext: rudderstackai.RequestContextOptions{Enabled: true, Intents: []string{"debug_delivery"}}})
			if late {
				add()
			}
			cs := connectStdio(t, s, "2025-06-18", nil)
			args := map[string]any{requestContext: map[string]any{
				"intent": " debug_delivery ", "user_sentiment": "negative", "call_reason": "mine", "custom": "untouched",
			}}
			callTool(t, cs, "own", args, nil)
			if mustJSON(got) != mustJSON(args) {
				t.Errorf("handler got %s, want %s", mustJSON(got), mustJSON(args))
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
		{"next-line", "\u0085debug_delivery\u0085", "debug_delivery"},
		{"ideographic-and-paragraph", "\u3000debug_delivery\u2029", "debug_delivery"},
		{"bom-prefix", "\ufeffdebug_delivery", "other"},
		{"file-separator-prefix", "\u001cdebug_delivery", "other"},
		{"next-line-only", "\u0085", ""},
		{"ideographic-whitespace", "\u3000 \t", ""},
		{"bom-only", "\ufeff", "other"},
		{"unknown", " unknown ", "other"},
		{"whitespace", " \t\n\u2003 ", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := &handlerSeen{}
			s := workspaceServer("intent-context", seen)
			r := wrap(t, s, Options{RequestContext: rudderstackai.RequestContextOptions{Enabled: true, Intents: []string{"debug_delivery"}}})
			cs := connectStdio(t, s, "2025-06-18", nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w", requestContext: map[string]any{"intent": tc.intent}}, nil)
			p := r.props(t, "mcp_tool_called")
			if got, present := p["intent"]; tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("intent = %v (present %t), want %q", got, present, tc.want)
			}
			if mustJSON(seen.last()) != `{"workspace_id":"w"}` {
				t.Errorf("handler got %s", seen.last())
			}
		})
	}
}
