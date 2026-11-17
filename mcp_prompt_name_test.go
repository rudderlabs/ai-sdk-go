package rudderstackai

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// prompt_name is the exact registered name, as in the Node.js and Python SDKs: never trimmed or
// capped like a wire value.
func TestMCPPromptNameIsExactRegisteredName(t *testing.T) {
	name := " audit\u0085" + strings.Repeat("p", 300)
	server := mcp.NewServer(&mcp.Implementation{Name: "prompt-name-mcp", Version: "1"}, nil)
	server.AddPrompt(&mcp.Prompt{Name: name}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	if _, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name}); err != nil {
		t.Fatal(err)
	}
	if got := r.props(t, "mcp_prompt_used")["prompt_name"]; got != name {
		t.Fatalf("prompt_name = %q, want the exact registered name", got)
	}
}
