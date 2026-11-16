package ruddermcpgo

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestMCPGoCallerModel(t *testing.T) {
	for name, connect := range map[string]func(*testing.T, *server.MCPServer) *sdk.ClientSession{
		"stdio": func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectStdio(t, s, "2025-11-25", nil)
		},
		"http": func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTP(t, s, "2025-11-25", nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"list_destinations": {Event: "destinations_listed"}}}})
			cs := connect(t, s)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, sdk.Meta{
				"x-codex-turn-metadata": map[string]any{
					"model":      "gpt-5.2-codex",
					"workspaces": map[string]any{"/Users/jane.doe/src/app": map[string]any{"has_changes": true}},
				},
				"io.modelcontextprotocol/aiInvocation": map[string]any{"userIntent": "refund my order"},
			})
			p := r.props(t, "mcp_tool_called")
			if p["caller_model"] != "gpt-5.2-codex" || p["caller_model_source"] != "client_metadata" {
				t.Fatalf("caller model not sent: %v", p)
			}
			if _, ok := r.props(t, "destinations_listed")["caller_model"]; ok {
				t.Fatal("caller_model on the declared tool event")
			}
			if sent := mustJSON(r.all()); strings.Contains(sent, "jane.doe") || strings.Contains(sent, "refund my order") {
				t.Fatalf("other _meta content sent: %s", sent)
			}
		})
	}
}

func TestMCPGoCallerModelAbsent(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	callTool(t, connectStdio(t, s, "2025-11-25", nil), "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	p := r.props(t, "mcp_tool_called")
	for _, key := range []string{"caller_model", "caller_model_source"} {
		if _, ok := p[key]; ok {
			t.Fatalf("%s without a model: %v", key, p)
		}
	}
}
