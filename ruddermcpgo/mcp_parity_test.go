package ruddermcpgo

import (
	"context"
	"slices"
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

func TestSubmittedCapabilitiesTrimmed(t *testing.T) {
	const whitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, feedback := range []bool{false, true} {
		tool, event := capabilityTool, "mcp_capability_requested"
		if feedback {
			tool, event = feedbackTool, "mcp_feedback_submitted"
		}
		t.Run(tool, func(t *testing.T) {
			for _, tc := range []struct {
				name, configured, submitted, want string
			}{
				{"matching", "bulk_export", "bulk_export", "bulk_export"},
				{"spaces-and-next-line", "bulk_export", " bulk_export\u0085", "bulk_export"},
				{"unicode-white-space", "bulk_export", whitespace + "bulk_export" + whitespace, "bulk_export"},
				{"file-separator-listed", "\u001cbulk_export\u001c", "\u001cbulk_export\u001c", "\u001cbulk_export\u001c"},
				{"bom-listed", "\ufeffbulk_export\ufeff", "\ufeffbulk_export\ufeff", "\ufeffbulk_export\ufeff"},
				{"file-separator", "bulk_export", "\u001cbulk_export\u001c", "other"},
				{"bom-prefix", "bulk_export", "\ufeffbulk_export", "other"},
				{"unknown", "bulk_export", " unknown ", "other"},
				{"empty", "bulk_export", "", "other"},
				{"whitespace-only", "bulk_export", whitespace, "other"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := workspaceServer("submitted-capabilities", nil)
					opts := Options{}
					if feedback {
						opts.FeedbackRequests = &rudderstackai.FeedbackRequestOptions{Capabilities: []string{tc.configured}}
					} else {
						opts.CapabilityRequests = &rudderstackai.CapabilityRequestOptions{Capabilities: []string{tc.configured}}
					}
					r := wrap(t, s, opts)
					cs := connectStdio(t, s, "2025-06-18", nil)
					tools, err := cs.ListTools(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					listed := false
					for _, listedTool := range tools.Tools {
						if listedTool.Name != tool {
							continue
						}
						listed = true
						props := listedTool.InputSchema.(map[string]any)["properties"].(map[string]any)
						got := toStrings(props["capability"].(map[string]any)["enum"])
						if want := []string{tc.configured, "other"}; !slices.Equal(got, want) {
							t.Errorf("capability enum = %q, want %q", got, want)
						}
					}
					if !listed {
						t.Fatalf("%s not offered", tool)
					}
					args := map[string]any{"capability": tc.submitted}
					if feedback {
						args["category"] = "feature_request"
					}
					if result := callTool(t, cs, tool, args, nil); result.IsError {
						t.Fatalf("%s failed: %s", tool, mustJSON(result))
					}
					if got := r.props(t, event)["capability"]; got != tc.want {
						t.Errorf("capability = %q, want %q", got, tc.want)
					}
					if got := r.named("mcp_tool_called"); len(got) != 0 {
						t.Errorf("unexpected mcp_tool_called events: %v", got)
					}
				})
			}
		})
	}
}
