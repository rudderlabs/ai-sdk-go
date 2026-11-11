package ruddermcpgo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPPrivacyRegistryAtCallTime(t *testing.T) {
	s := workspaceServer("mcpgo-privacy-registry", nil)
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", nil)
	s.DeleteTools("list_destinations")
	_, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_destinations"})
	if p := r.named("mcp_tool_called")[0].Properties; p["tool_name"] != "unknown" {
		t.Errorf("removed tool = %v", p)
	}
	s.AddTool(mcp.NewTool("late", mcp.WithString("kept")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	_, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "late", Arguments: map[string]any{"kept": "ok", "jane.doe@acme.com": 1}})
	if p := r.named("mcp_tool_called")[1].Properties; p["tool_name"] != "late" || !slices.Equal(p["argument_keys"].([]string), []string{"unknown", "kept"}) {
		t.Errorf("late tool = %v", p)
	}
}

func TestMCPPrivacyWireIDs(t *testing.T) {
	s := workspaceServer("mcpgo-privacy-ids", nil)
	r := wrap(t, s, Options{})
	for i, id := range []any{"SELECT email FROM customers", "req-1", 7, strings.Repeat("a", 65), 1.5, true} {
		// Fractions and booleans are not usable ids: "unknown", not a hash.
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}}})
		_ = s.HandleMessage(context.Background(), raw)
		p := r.named("mcp_tool_called")[i].Properties
		got, present := p["request_id"]
		want := map[int]string{1: "req-1", 2: "7", 4: "unknown", 5: "unknown"}[i]
		if want == "" {
			raw, _ := json.Marshal(id)
			sum := sha256.Sum256(func() []byte {
				if str, ok := id.(string); ok {
					return []byte(str)
				}
				return raw
			}())
			want = "sha256:" + hex.EncodeToString(sum[:])[:16]
		}
		if got != want || !present {
			t.Errorf("id %v = %v, want %s", id, p, want)
		}
	}
}
