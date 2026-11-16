package ruddermcpgo

import (
	"context"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// SDK-6888: an unset CaptureContent follows the global setting; ContentCapture decides per kind.
func TestContentCaptureFollowsGlobalAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
		opts   Options
		reason bool
	}{
		{"unset inherits global on", true, Options{}, true},
		{"unset inherits global off", false, Options{}, false},
		{"explicit off over global on", true, Options{ContentCapture: &rudderstackai.ContentCapture{Default: rudderstackai.Ptr(false)}}, false},
		{"call reason only", false, Options{ContentCapture: &rudderstackai.ContentCapture{CallReason: rudderstackai.Ptr(true)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(tc.global)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithCaptureContent(false)) })
			s := workspaceServer("capture-policy-mcpgo", nil)
			tc.opts.RequestContext = rudderstackai.RequestContextOptions{Enabled: true}
			r := wrap(t, s, tc.opts)
			cs := connectStdio(t, s, "2025-06-18", nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w", requestContext: map[string]any{"call_reason": "check delivery"}}, nil)
			if _, got := r.props(t, "mcp_tool_called")["call_reason"]; got != tc.reason {
				t.Errorf("call_reason sent = %v, want %v", got, tc.reason)
			}
		})
	}
}

// The injected submit_feedback schema follows the live policy: a call matching the advertised
// schema succeeds after the global setting changes, in both directions.
func TestFeedbackSchemaFollowsLiveGlobalPolicy(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "off then on", true: "on then off"}[before], func(t *testing.T) {
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(before)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithCaptureContent(false)) })
			s := workspaceServer("feedback-live-mcpgo", nil)
			r := wrap(t, s, Options{FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}})
			cs := connectStdio(t, s, "2025-06-18", nil)
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(!before)); err != nil {
				t.Fatal(err)
			}
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			for _, tool := range tools.Tools {
				if tool.Name == feedbackTool {
					schema = tool.InputSchema.(map[string]any)
				}
			}
			_, offered := schema["properties"].(map[string]any)["feedback"]
			if offered != !before {
				t.Fatalf("feedback offered = %v after turning capture %v", offered, !before)
			}
			args := map[string]any{"category": "bug_report"}
			if offered {
				args["feedback"] = "The export button does nothing."
			}
			if res := callTool(t, cs, feedbackTool, args, nil); res.IsError {
				t.Fatalf("call matching the advertised schema rejected: %s", mustJSON(res))
			}
			if _, sent := r.props(t, "mcp_feedback_submitted")["feedback_text"]; sent != offered {
				t.Fatalf("feedback_text sent = %v, want %v", sent, offered)
			}
		})
	}
}
