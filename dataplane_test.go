package rudderstackai

import (
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

func TestEventsReachTheDataPlane(t *testing.T) {
	dp := dataplane.Start()
	defer dp.Close()
	client, err := analytics.NewWithConfig("write-key", analytics.Config{DataPlaneUrl: dp.URL, BatchSize: 1, NoProxySupport: true, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resetMemory)
	server := workspaceServer("dp-mcp", nil)
	if err := WrapMCPServer(server, Options{Analytics: client}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	msgs := dp.Wait(2, 5*time.Second)
	if len(msgs) != 2 {
		t.Fatalf("data plane got %d messages", len(msgs))
	}
	for _, m := range msgs {
		if m.Type != "track" || m.Channel != "server" || m.AnonymousID != "ai_system" {
			t.Errorf("message = %+v", m)
		}
		lib, _ := m.Context["library"].(map[string]any)
		wl, _ := m.Context["wrapperLibrary"].(map[string]any)
		if lib["name"] != "analytics-go" {
			t.Errorf("context.library = %v", lib)
		}
		if wl["name"] != "ai-sdk-go" || wl["version"] != Version || wl["specVersion"] != "0.3.0" || wl["integration"] != "mcp" {
			t.Errorf("context.wrapperLibrary = %v", wl)
		}
		if err := validateEvent(m.Event, m.Properties); err != nil {
			t.Errorf("%s: %v", m.Event, err)
		}
	}
	if msgs[0].Event != "mcp_client_seen" || msgs[1].Event != "mcp_tool_called" {
		t.Errorf("events = %s, %s", msgs[0].Event, msgs[1].Event)
	}
}
