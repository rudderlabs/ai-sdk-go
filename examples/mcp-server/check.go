package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// mainEnv makes a test binary run main instead of its tests, so the check can start this
// program as a stdio server from `go test` too.
const mainEnv = "MCP_SERVER_EXAMPLE_MAIN"

// lastMessages keeps the messages of the last check, for the tests' schema validation.
var lastMessages []dataplane.Message

func runCheck(stderr io.Writer) error {
	dp := dataplane.Start()
	defer dp.Close()
	var all []dataplane.Message
	for _, leg := range []struct {
		name string
		run  func(string, io.Writer) error
	}{
		{"streamable HTTP, 2025-06-18", func(url string, _ io.Writer) error { return httpLeg(url, "2025-06-18", "check-client-2025") }},
		{"streamable HTTP, 2026-07-28", func(url string, _ io.Writer) error { return httpLeg(url, "", "check-client-2026") }},
		{"stdio", stdioLeg},
	} {
		before := len(dp.Messages())
		if err := leg.run(dp.URL, stderr); err != nil {
			return fmt.Errorf("%s: %w", leg.name, err)
		}
		msgs := dp.Messages()[before:]
		if err := expectEvents(msgs, leg.name); err != nil {
			return fmt.Errorf("%s: %w", leg.name, err)
		}
		fmt.Fprintf(stderr, "ok  %s: %d events\n", leg.name, len(msgs))
		all = append(all, msgs...)
	}
	lastMessages = all
	return nil
}

// exercise lists the tools and calls each kind once, as an agent would.
func exercise(ctx context.Context, cs *mcp.ClientSession) error {
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(tools.Tools, func(t *mcp.Tool) bool { return t.Name == "request_capability" }) {
		return fmt.Errorf("request_capability not offered")
	}
	meta := mcp.Meta{"com.rudderstack/interaction_id": "int_check", "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	calls := []*mcp.CallToolParams{
		{Name: "list_destinations", Meta: meta, Arguments: map[string]any{
			"workspace_id": "ws_1", "request_context": map[string]any{"intent": "debug_delivery", "user_sentiment": "negative"},
		}},
		{Name: "create_audience", Meta: meta, Arguments: map[string]any{"name": "vip"}},
		{Name: "request_capability", Meta: meta, Arguments: map[string]any{"capability": "bulk_export"}},
	}
	for _, c := range calls {
		res, err := cs.CallTool(ctx, c)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		if res.IsError {
			return fmt.Errorf("%s returned an error result", c.Name)
		}
	}
	return nil
}

// httpLeg runs one client over streamable HTTP. Each leg names its client differently:
// mcp_client_seen reports a client once per window.
func httpLeg(dataPlaneURL, protocol, clientName string) error {
	client, err := analytics.NewWithConfig("check", analytics.Config{DataPlaneUrl: dataPlaneURL, NoProxySupport: true, Interval: 20 * time.Millisecond})
	if err != nil {
		return err
	}
	server, err := newServer(client, "")
	if err != nil {
		return err
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer ts.Close()
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.0.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		return err
	}
	err = exercise(ctx, cs)
	_ = cs.Close()
	if cerr := client.Close(); err == nil {
		err = cerr
	}
	return err
}

func stdioLeg(dataPlaneURL string, stderr io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "--transport=stdio")
	cmd.Env = append(os.Environ(), "RUDDERSTACK_WRITE_KEY=check", "RUDDERSTACK_DATA_PLANE_URL="+dataPlaneURL, mainEnv+"=1")
	cmd.Stderr = stderr
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "check-client", Version: "1.0.0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		return err
	}
	err = exercise(ctx, cs)
	// Closing stdin stops the server, which flushes its events before exiting.
	if cerr := cs.Close(); err == nil && cerr != nil && !strings.Contains(cerr.Error(), "exit") {
		err = cerr
	}
	return err
}

// expectEvents checks the events one exercise must produce.
func expectEvents(msgs []dataplane.Message, leg string) error {
	byName := map[string][]dataplane.Message{}
	for _, m := range msgs {
		wl, _ := m.Context["wrapperLibrary"].(map[string]any)
		lib, _ := m.Context["library"].(map[string]any)
		if wl["name"] != "ai-sdk-go" || wl["integration"] != "mcp" || wl["specVersion"] != "0.3.0" || lib["name"] != "analytics-go" {
			return fmt.Errorf("%s: context = %v", m.Event, m.Context)
		}
		byName[m.Event] = append(byName[m.Event], m)
	}
	want := map[string]int{"mcp_client_seen": 1, "mcp_tool_called": 2, "audience_created": 1, "mcp_capability_requested": 1}
	for name, n := range want {
		if len(byName[name]) != n {
			return fmt.Errorf("got %d %s, want %d (events: %d)", len(byName[name]), name, n, len(msgs))
		}
	}
	transport := "streamable_http"
	if leg == "stdio" {
		transport = "stdio"
	}
	if got := byName["mcp_client_seen"][0].Properties["transport"]; got != transport {
		return fmt.Errorf("transport = %v, want %s", got, transport)
	}
	list := byName["mcp_tool_called"][0].Properties
	checks := map[string]any{
		"tool_name": "list_destinations", "tool_status": "success", "interaction_id": "int_check",
		"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "intent": "debug_delivery", "user_sentiment": "negative",
		"server_name": "workspace-mcp",
	}
	for k, v := range checks {
		if list[k] != v {
			return fmt.Errorf("mcp_tool_called %s = %v, want %v", k, list[k], v)
		}
	}
	if name, _ := list["client_name"].(string); !strings.HasPrefix(name, "check-client") {
		return fmt.Errorf("mcp_tool_called client_name = %v", list["client_name"])
	}
	if got := byName["audience_created"][0].Properties["audience_type"]; got != "computed" {
		return fmt.Errorf("audience_created audience_type = %v", got)
	}
	return nil
}
