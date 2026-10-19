//go:build mcpgo_v1

// Multi round-trip requests (input_required) exist from mcp-go v1.0. go.mod requires the oldest
// supported version, v0.55.1; the CI job that tests the latest mcp-go runs with -tags mcpgo_v1.

package ruddermcpgo

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A 2026-07-28 tool that needs confirmation returns input_required; the client's retry answers.
func TestInputRequired2026(t *testing.T) {
	s := server.NewMCPServer("confirm-mcp", "2.0.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("confirm"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if answer := server.ElicitationResponse(req.Params.InputResponses, "ok"); answer != nil {
			return mcp.NewToolResultText("done: " + string(answer.Action)), nil
		}
		return server.NewInputRequestBuilder("state-1").Elicit("ok", mcp.ElicitationParams{
			Message: "Create the secret audience?", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}).ToolResult(), nil
	})
	r := wrap(t, s, Options{})
	cs := connectHTTP(t, s, "", &sdk.ClientOptions{ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "decline", Content: map[string]any{}}, nil
	}})
	if got := cs.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated %s", got)
	}
	res := callTool(t, cs, "confirm", map[string]any{}, sdk.Meta{metaInteractionID: "int_2026"})
	if text := res.Content[0].(*sdk.TextContent).Text; text != "done: decline" {
		t.Fatalf("result = %s", text)
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 1 {
		t.Fatalf("mcp_tool_called = %d, want 1 (the input_required leg sends none): %v", len(calls), r.names())
	}
	if _, ok := calls[0].Properties["retry_count"]; ok {
		t.Error("retry_count on the retry that answers the server")
	}
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "decline" || p["tool_name"] != "confirm" || p["interaction_id"] != "int_2026" || p["duration_ms"] == nil {
		t.Errorf("elicitation = %v", p)
	}
	if p["request_id"] == nil || p["request_id"] == calls[0].Properties["request_id"] {
		t.Errorf("request_id = %v, want the original request's", p["request_id"])
	}
}
