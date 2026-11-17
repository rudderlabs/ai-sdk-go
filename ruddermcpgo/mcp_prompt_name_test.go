package ruddermcpgo

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// prompt_name is the exact registered name, as in the Node.js and Python SDKs.
func TestMCPGoPromptNameIsExactRegisteredName(t *testing.T) {
	name := " audit\u0085" + strings.Repeat("p", 300)
	s := server.NewMCPServer("prompt-name-mcp", "1", server.WithPromptCapabilities(false))
	s.AddPrompt(mcp.NewPrompt(name), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return mcp.NewGetPromptResult("p", nil), nil
	})
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", nil)
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name}); err != nil {
		t.Fatal(err)
	}
	if got := r.props(t, "mcp_prompt_used")["prompt_name"]; got != name {
		t.Fatalf("prompt_name = %q, want the exact registered name", got)
	}
}
