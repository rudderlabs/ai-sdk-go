package ruddermcpgo

import (
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
