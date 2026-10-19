package main

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// mainEnv makes a test binary run main instead of its tests, so the check can start this
// program as a stdio server from `go test` too.
const mainEnv = "MCP_SERVER_MCPGO_EXAMPLE_MAIN"

// runs numbers the checks in this process, so a second run (--check, then --live, in one test
// binary) uses new client names.
var runs atomic.Int64

// runCheck runs three legs. Each uses its own client name and interaction ID: state is kept per
// server name for the whole process, and every leg serves "workspace-mcp".
func runCheck(sink *harness.Sink, stderr io.Writer) error {
	suffix := ""
	if n := runs.Add(1); n > 1 {
		suffix = "-" + strconv.FormatInt(n, 10)
	}
	for _, leg := range []struct {
		name, client string
		run          func(*harness.Sink, string) (string, error)
	}{
		{"streamable HTTP, 2025-06-18", "check-client-2025", func(s *harness.Sink, c string) (string, error) { return httpLeg(s, "2025-06-18", c) }},
		{"streamable HTTP, latest client", "check-client-latest", func(s *harness.Sink, c string) (string, error) { return httpLeg(s, "", c) }},
		{"stdio", "check-client-stdio", func(s *harness.Sink, c string) (string, error) { return stdioLeg(s, c, stderr) }},
	} {
		before := len(sink.Messages())
		client := leg.client + suffix
		protocol, err := leg.run(sink, client)
		if err != nil {
			return fmt.Errorf("%s: %w", leg.name, err)
		}
		msgs := sink.Messages()[before:]
		if err := expectEvents(msgs, leg.name == "stdio", client, protocol); err != nil {
			return fmt.Errorf("%s: %w", leg.name, err)
		}
		fmt.Fprintf(stderr, "ok  %s (%s): %d events\n", leg.name, protocol, len(msgs))
	}
	return nil
}

const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// exercise lists the tools and calls each kind once, as an agent would.
func exercise(ctx context.Context, cs *sdk.ClientSession, clientName string) error {
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(tools.Tools, func(t *sdk.Tool) bool { return t.Name == "request_capability" }) {
		return fmt.Errorf("request_capability not offered")
	}
	meta := sdk.Meta{"com.rudderstack/interaction_id": "int_" + clientName, "traceparent": traceparent}
	calls := []*sdk.CallToolParams{
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

// httpLeg runs one client over streamable HTTP and returns the protocol version it negotiated.
func httpLeg(sink *harness.Sink, protocol, clientName string) (string, error) {
	client, err := sink.NewClient()
	if err != nil {
		return "", err
	}
	s, err := newServer(client, "")
	if err != nil {
		return "", err
	}
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	defer ts.Close()
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: "1.0.0"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, &sdk.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		return "", err
	}
	err = exercise(ctx, cs, clientName)
	_ = cs.Close()
	if cerr := client.Close(); err == nil {
		err = cerr
	}
	return cs.InitializeResult().ProtocolVersion, err
}

func stdioLeg(sink *harness.Sink, clientName string, stderr io.Writer) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(self, "--transport=stdio")
	cmd.Env = append(os.Environ(), "RUDDERSTACK_WRITE_KEY="+sink.WriteKey, "RUDDERSTACK_DATA_PLANE_URL="+sink.URL, mainEnv+"=1")
	cmd.Stderr = stderr
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: "1.0.0"}, nil).
		Connect(ctx, &sdk.CommandTransport{Command: cmd}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		return "", err
	}
	err = exercise(ctx, cs, clientName)
	// Closing stdin stops the server, which flushes its events before exiting.
	if cerr := cs.Close(); err == nil && cerr != nil && !strings.Contains(cerr.Error(), "exit") {
		err = cerr
	}
	return cs.InitializeResult().ProtocolVersion, err
}

// expectEvents checks the events one exercise must produce, field by field.
func expectEvents(msgs []dataplane.Message, stdio bool, clientName, protocol string) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{
		"mcp_client_seen": 1, "mcp_tool_called": 2, "audience_created": 1, "mcp_capability_requested": 1,
	}); err != nil {
		return err
	}
	transport := "streamable_http"
	if stdio {
		transport = "stdio"
	}
	interaction := "int_" + clientName
	list := harness.Find(byEvent["mcp_tool_called"], "tool_name", "list_destinations")
	// Every event, mcp_client_seen included, is for the session the calls ran in; stdio and
	// stateless 2026-07-28 requests have none.
	var session any = harness.Absent
	if id, ok := list["mcp_session_id"]; ok {
		session = id
	} else if !stdio && protocol != "2026-07-28" {
		return fmt.Errorf("mcp_tool_called without mcp_session_id over streamable HTTP")
	}
	checks := []struct {
		event string
		props map[string]any
		want  map[string]any
	}{
		{"mcp_client_seen", byEvent["mcp_client_seen"][0].Properties, map[string]any{
			"server_name": "workspace-mcp", "server_version": "1.4.0", "client_name": clientName, "client_version": "1.0.0",
			"transport": transport, "protocol_version": protocol, "auth_method": "none",
		}},
		{"mcp_tool_called", list, map[string]any{
			"tool_name": "list_destinations", "tool_status": "success", "server_name": "workspace-mcp", "client_name": clientName,
			"interaction_id": interaction, "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "intent": "debug_delivery",
			"user_sentiment": "negative", "argument_keys": []string{"workspace_id"}, "is_error_result": false,
			"call_index": 0, "retry_count": 0, "protocol_version": protocol, "duration_ms": harness.Present, "request_id": harness.Present,
		}},
		{"mcp_tool_called", harness.Find(byEvent["mcp_tool_called"], "tool_name", "create_audience"), map[string]any{
			"tool_name": "create_audience", "tool_status": "success", "interaction_id": interaction, "call_index": 1,
			"argument_keys": []string{"name"}, "intent": harness.Absent,
		}},
		{"audience_created", byEvent["audience_created"][0].Properties, map[string]any{
			"audience_type": "computed", "tool_name": "create_audience", "tool_status": "success", "interaction_id": interaction,
		}},
		{"mcp_capability_requested", byEvent["mcp_capability_requested"][0].Properties, map[string]any{
			"capability": "bulk_export", "interaction_id": interaction, "server_name": "workspace-mcp",
		}},
	}
	for _, c := range checks {
		c.want["mcp_session_id"] = session
		if err := harness.Fields(c.event, c.props, c.want); err != nil {
			return err
		}
	}
	for _, m := range msgs {
		if m.UserID != "" || m.AnonymousID != "ai_system" {
			return fmt.Errorf("%s: userId %q, anonymousId %q; want the anonymous system user", m.Event, m.UserID, m.AnonymousID)
		}
	}
	return nil
}
