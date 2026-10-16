package main

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

const protocol = "2026-07-28"

// runCheck serves one long-lived server to two agents, one that confirms and one that
// declines. Each has its own client name and interaction ID, so the asserts hold whether the
// SDK keeps client windows and counters per wrapped server or per server name.
func runCheck(sink *harness.Sink, stderr io.Writer) error {
	client, err := sink.NewClient()
	if err != nil {
		return err
	}
	h, err := newHandler(client)
	if err != nil {
		return err
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	legs := []struct{ client, action, result string }{
		{"agent-confirms", "accept", "deleted"},
		{"agent-declines", "decline", "kept"},
	}
	for _, leg := range legs {
		if err := agent(ts.URL, leg.client, leg.action, leg.result); err != nil {
			return fmt.Errorf("%s: %w", leg.client, err)
		}
	}
	// Close flushes every queued event before the asserts.
	if err := client.Close(); err != nil {
		return err
	}
	all := sink.Messages()
	for _, leg := range legs {
		var msgs []dataplane.Message
		for _, m := range all {
			if m.Properties["client_name"] == leg.client {
				msgs = append(msgs, m)
			}
		}
		if err := expectEvents(msgs, leg.client, leg.action); err != nil {
			return fmt.Errorf("%s: %w", leg.client, err)
		}
		fmt.Fprintf(stderr, "ok  %s: %d events\n", leg.client, len(msgs))
	}
	if len(all) != 6 {
		return fmt.Errorf("got %d events, want 6", len(all))
	}
	return nil
}

// agent connects a go-sdk client that answers the confirmation with action, and deletes an
// audience. The SDK sends the call, receives input_required, asks the elicitation handler and
// retries with the answer.
func agent(url, name, action, want string) error {
	ctx := context.Background()
	opts := &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: action}, nil
	}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: name, Version: "3.1.0"}, opts).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		return err
	}
	defer cs.Close()
	if got := cs.InitializeResult().ProtocolVersion; got != protocol {
		return fmt.Errorf("negotiated %s, want %s", got, protocol)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "delete_audience", Arguments: map[string]any{"audience_id": "aud_42"},
		Meta: mcp.Meta{"com.rudderstack/interaction_id": "int_" + name},
	})
	if err != nil {
		return err
	}
	if text := res.Content[0].(*mcp.TextContent).Text; text != want {
		return fmt.Errorf("result %q, want %q", text, want)
	}
	return nil
}

// expectEvents checks one agent's events field by field: the input_required leg sends no
// mcp_tool_called, the retry sends one, and the answer is an mcp_elicitation_completed tied
// to the original request.
func expectEvents(msgs []dataplane.Message, client, action string) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"mcp_client_seen": 1, "mcp_tool_called": 1, "mcp_elicitation_completed": 1}); err != nil {
		return err
	}
	common := map[string]any{
		"server_name": "audience-admin-mcp", "client_name": client, "client_version": "3.1.0",
		"protocol_version": protocol, "mcp_session_id": harness.Absent,
	}
	seen := byEvent["mcp_client_seen"][0].Properties
	call := byEvent["mcp_tool_called"][0].Properties
	answer := byEvent["mcp_elicitation_completed"][0].Properties
	checks := []struct {
		event string
		props map[string]any
		want  map[string]any
	}{
		{"mcp_client_seen", seen, map[string]any{"server_version": "2.0.0", "transport": "streamable_http", "auth_method": "none"}},
		{"mcp_tool_called", call, map[string]any{
			"tool_name": "delete_audience", "tool_status": "success", "interaction_id": "int_" + client,
			"argument_keys": []string{"audience_id"}, "call_index": 1, "retry_count": harness.Absent,
			"is_error_result": false, "request_id": harness.Present, "duration_ms": harness.Present,
		}},
		{"mcp_elicitation_completed", answer, map[string]any{
			"elicitation_action": action, "tool_name": "delete_audience", "interaction_id": "int_" + client,
			"duration_ms": harness.Present, "request_id": harness.Present, "error_type": harness.Absent,
		}},
	}
	for _, c := range checks {
		if err := harness.Fields(c.event, c.props, common); err != nil {
			return err
		}
		if err := harness.Fields(c.event, c.props, c.want); err != nil {
			return err
		}
	}
	if answer["request_id"] == call["request_id"] {
		return fmt.Errorf("mcp_elicitation_completed.request_id = the retry's %v, want the original request's", call["request_id"])
	}
	for _, m := range msgs {
		if m.UserID != "" || m.AnonymousID != "ai_system" {
			return fmt.Errorf("%s: userId %q, anonymousId %q; want the anonymous system user", m.Event, m.UserID, m.AnonymousID)
		}
	}
	return nil
}
