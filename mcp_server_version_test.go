package rudderstackai

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A rewrap normalises the server's own version again, not the already-capped one, so a 256
// code-point cap that ends in white space keeps it.
func TestMCPServerVersionNormalizedOnceAcrossRewrap(t *testing.T) {
	version := strings.Repeat("😀", 255) + "\u0085tail"
	s := mcp.NewServer(&mcp.Implementation{Name: t.Name(), Version: version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "lookup"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	r := wrap(t, s, Options{})
	if err := WrapMCPServer(s, Options{Analytics: r}); err != nil { // rewrap
		t.Fatal(err)
	}
	cs := connect(t, s, "2025-11-25", nil)
	callTool(t, cs, "lookup", map[string]any{}, nil)
	got, _ := r.props(t, "mcp_tool_called")["server_version"].(string)
	if n := utf8.RuneCountInString(got); n != 256 || !strings.HasSuffix(got, "\u0085") {
		t.Fatalf("server_version has %d code points, want 256 ending in U+0085", n)
	}
}
