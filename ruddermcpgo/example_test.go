package ruddermcpgo_test

import (
	"context"
	"log"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddermcpgo"
)

type workspaceKey struct{}

func ExampleWrapServer() {
	client, err := analytics.NewWithConfig("WRITE_KEY", analytics.Config{DataPlaneUrl: "https://example.dataplane.rudderstack.com"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close() // flushes queued events

	s := server.NewMCPServer("workspace-mcp", "1.4.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("create_audience", mcp.WithString("name", mcp.Required())),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(`{"id":"aud_1"}`), nil
		})

	err = ruddermcpgo.WrapServer(s, ruddermcpgo.Options{
		Analytics: client,
		// Your own auth puts the workspace in the context, for example with
		// server.WithHTTPContextFunc.
		Identify: func(ctx context.Context) (rudderstackai.Identity, error) {
			workspace, _ := ctx.Value(workspaceKey{}).(string)
			return rudderstackai.Identity{AccountID: workspace}, nil
		},
		EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
			"create_audience": {Event: "audience_created"},
		}},
		RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	// Serve as usual: server.ServeStdio(s) or server.NewStreamableHTTPServer(s).
}
