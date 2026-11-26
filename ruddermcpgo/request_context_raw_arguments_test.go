package ruddermcpgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestWireRequestContextStripsRawArguments(t *testing.T) {
	s := server.NewMCPServer("raw-context", "1", server.WithToolCapabilities(false))
	called := false
	s.AddTool(mcp.NewTool("lookup", mcp.WithString("workspace_id")), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		var bound map[string]any
		if err := req.BindArguments(&bound); err != nil {
			return nil, err
		}
		for name, args := range map[string]map[string]any{"GetArguments": req.GetArguments(), "BindArguments": bound} {
			if got := mustJSON(args); got != `{"workspace_id":"w"}` {
				t.Errorf("%s = %s, want only workspace_id", name, got)
			}
		}
		raw, err := json.Marshal(req.GetRawArguments())
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var typed struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := decoder.Decode(&typed); err != nil {
			return nil, err
		}
		if typed.WorkspaceID != "w" {
			return nil, fmt.Errorf("raw workspace_id = %q, want w", typed.WorkspaceID)
		}
		var params struct {
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(mustJSON(req.Params)), &params); err != nil {
			return nil, err
		}
		if got := mustJSON(params.Arguments); got != `{"workspace_id":"w"}` {
			t.Errorf("marshaled arguments = %s, want only workspace_id", got)
		}
		return mcp.NewToolResultText("ok"), nil
	})
	r := wrap(t, s, Options{RequestContext: rudderstackai.RequestContextOptions{Enabled: true, Intents: []string{"support"}}})
	wire := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"workspace_id":"w","request_context":{"intent":"support","call_reason":"private purpose"}}}}`)
	response := s.HandleMessage(context.Background(), wire)
	if !called {
		t.Fatal("tool handler was not called")
	}
	var decoded struct {
		Error  json.RawMessage     `json:"error"`
		Result *mcp.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(mustJSON(response)), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Error) != 0 || decoded.Result == nil || decoded.Result.IsError {
		t.Fatalf("tool call failed: %s", mustJSON(response))
	}
	if got := r.props(t, "mcp_tool_called")["tool_status"]; got != "success" {
		t.Errorf("tool_status = %v, want success", got)
	}
}
