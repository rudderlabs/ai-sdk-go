package ruddermcpgo

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func lastToolProps(t *testing.T, r *recorder) map[string]any {
	t.Helper()
	events := r.named("mcp_tool_called")
	return events[len(events)-1].Properties
}

// Names come from what the server would serve this session now: task tools and the session's
// own tools (and their schemas) count, never an earlier tools/list.
func TestRegistryIncludesTaskAndSessionTools(t *testing.T) {
	s := workspaceServer("mcpgo-hardening-registry", nil)
	r := wrap(t, s, Options{})
	cs := connectHTTP(t, s, "2025-06-18", nil)
	s.AddTaskTool(mcp.NewTool("task_tool", mcp.WithString("input")), func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
		return &mcp.CreateTaskResult{}, nil
	})
	if err := s.AddSessionTool(cs.ID(), mcp.NewTool("session_tool", mcp.WithString("mine")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}); err != nil {
		t.Fatal(err)
	}
	// The session overrides a global tool's schema: its own property is registered, the global one is not.
	if err := s.AddSessionTools(cs.ID(), server.ServerTool{Tool: mcp.NewTool("list_destinations", mcp.WithString("session_only")), Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"task_tool", map[string]any{"input": "x", "evil": 1}, []string{"unknown", "input"}},
		{"session_tool", map[string]any{"mine": "x", "evil": 1}, []string{"unknown", "mine"}},
		{"list_destinations", map[string]any{"session_only": "x", "workspace_id": "w"}, []string{"session_only", "unknown"}},
	} {
		_, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if p := lastToolProps(t, r); p["tool_name"] != tc.tool || !slices.Equal(p["argument_keys"].([]string), tc.want) {
			t.Errorf("%s = %v", tc.tool, p)
		}
	}
}

func TestPromptNamesAreRegistered(t *testing.T) {
	s := workspaceServer("mcpgo-hardening-prompts", nil)
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", nil)
	_, _ = cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "audit_tracking_plan"})
	_, _ = cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "jane.doe@acme.com"})
	events := r.named("mcp_prompt_used")
	if len(events) != 2 || events[0].Properties["prompt_name"] != "audit_tracking_plan" || events[1].Properties["prompt_name"] != "unknown" {
		t.Errorf("prompt events = %v", mustJSON(events))
	}
}

// The mcp-go server rejects a non-date version before any hook, so only the HTTP header
// fallback is reachable here; the shared engine tests cover "evil".
func TestProtocolVersionFromHTTPHeader(t *testing.T) {
	s := workspaceServer("mcpgo-hardening-version", nil)
	r := wrap(t, s, Options{})
	cs := connectHTTP(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if got := lastToolProps(t, r)["protocol_version"]; got != "2025-06-18" {
		t.Errorf("protocol_version = %v", got)
	}
}

// The mcp-go server hands the hooks ids as it decoded them; each is encoded by the shared rule.
func TestWireIDEdgeCases(t *testing.T) {
	s := workspaceServer("mcpgo-hardening-ids", nil)
	r := wrap(t, s, Options{})
	for i, tc := range []struct{ raw, want string }{
		{`7`, "7"}, {`7.0`, "7"}, {`-0`, "0"}, {`"7"`, "sha256:7902699be42c8a8e"}, {`"req-1"`, "sha256:9456bdfa12ea7695"}, {`1e21`, "unknown"}, {`1.5`, "unknown"},
		{`9007199254740993`, "unknown"}, {`""`, "unknown"}, {`true`, "unknown"}, {`"SELECT email FROM customers"`, "sha256:409abf7beb528a87"},
	} {
		msg := `{"jsonrpc":"2.0","id":` + tc.raw + `,"method":"tools/call","params":{"name":"list_destinations","arguments":{"workspace_id":"w"}}}`
		_ = s.HandleMessage(context.Background(), []byte(msg))
		events := r.named("mcp_tool_called")
		if len(events) != i+1 || events[i].Properties["request_id"] != tc.want {
			t.Errorf("id %s: got %v, want %s", tc.raw, events[len(events)-1].Properties["request_id"], tc.want)
		}
	}
	// No id is a notification: no event, no property.
	before := len(r.named("mcp_tool_called"))
	_ = s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"list_destinations"}}`))
	if got := len(r.named("mcp_tool_called")); got != before {
		t.Errorf("notification sent %d events", got-before)
	}
}

func TestLongCallerIDsKeepSeparateCounters(t *testing.T) {
	prefix := strings.Repeat("u", 300)
	s := workspaceServer("mcpgo-hardening-long", nil)
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", nil)
	// Same connection, interaction ids equal for 300 characters.
	for _, suffix := range []string{"one", "two"} {
		callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, sdk.Meta{"com.rudderstack/interaction_id": prefix + suffix})
		if got := lastToolProps(t, r)["call_index"]; got != 0 {
			t.Errorf("interaction %s: call_index = %v, want 0", suffix, got)
		}
	}
}
