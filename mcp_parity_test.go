package rudderstackai

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConfiguredMCPLabelsTrimmed(t *testing.T) {
	const whitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, tc := range []struct {
		name, whitespace string
		feedback         bool
	}{
		{"capability/spaces", " ", false},
		{"capability/unicode-white-space", whitespace, false},
		{"feedback/spaces", " ", true},
		{"feedback/unicode-white-space", whitespace, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := func(label string) []string {
				return []string{tc.whitespace + label + tc.whitespace, tc.whitespace + label, tc.whitespace, " other "}
			}
			s := workspaceServer("configured-labels", nil)
			opts := Options{RequestContext: RequestContextOptions{Enabled: true, Intents: labels("debug_delivery")}}
			if tc.feedback {
				opts.FeedbackRequests = &FeedbackRequestOptions{Capabilities: labels("bulk_export")}
			} else {
				opts.CapabilityRequests = &CapabilityRequestOptions{Capabilities: labels("bulk_export")}
			}
			r := wrap(t, s, opts)
			cs := connect(t, s, "2025-06-18", nil)
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			checked := map[string]bool{}
			for _, tool := range tools.Tools {
				props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
				var enum any
				var want []string
				switch tool.Name {
				case "list_destinations":
					contextProps := props[requestContext].(map[string]any)["properties"].(map[string]any)
					enum = contextProps["intent"].(map[string]any)["enum"]
					want = []string{"debug_delivery", "other"}
				case capabilityTool, feedbackTool:
					enum = props["capability"].(map[string]any)["enum"]
					want = []string{"bulk_export", "other"}
				default:
					continue
				}
				checked[tool.Name] = true
				if got := toStrings(enum); !slices.Equal(got, want) {
					t.Errorf("%s enum = %q, want %q", tool.Name, got, want)
				}
			}
			if len(checked) != 2 {
				t.Errorf("checked tools = %v, want intent and capability enums", checked)
			}
			callTool(t, cs, "list_destinations", map[string]any{
				"workspace_id": "w", requestContext: map[string]any{"intent": "debug_delivery"},
			}, nil)
			if got := r.props(t, "mcp_tool_called")["intent"]; got != "debug_delivery" {
				t.Errorf("intent = %v, want debug_delivery", got)
			}
		})
	}
}

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
