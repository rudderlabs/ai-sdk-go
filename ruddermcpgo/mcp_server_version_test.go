package ruddermcpgo

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// A rewrap normalises the server's own version again, not the already-capped one.
func TestMCPGoServerVersionNormalizedOnceAcrossRewrap(t *testing.T) {
	s := server.NewMCPServer(t.Name(), strings.Repeat("😀", 255)+"\u0085tail", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	r := wrap(t, s, Options{})
	if err := WrapServer(s, Options{Analytics: r}); err != nil { // rewrap
		t.Fatal(err)
	}
	callTool(t, connectStdio(t, s, "2025-11-25", nil), "lookup", map[string]any{}, nil)
	got, _ := r.props(t, "mcp_tool_called")["server_version"].(string)
	if n := utf8.RuneCountInString(got); n != 256 || !strings.HasSuffix(got, "\u0085") {
		t.Fatalf("server_version has %d code points, want 256 ending in U+0085", n)
	}
}
