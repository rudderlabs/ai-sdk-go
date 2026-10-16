// Command mcp-server-mcpgo is an MCP server built on github.com/mark3labs/mcp-go and tracked
// with ruddermcpgo, served over stdio or streamable HTTP.
//
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-server-mcpgo --transport=http
//
// With --check it runs end to end against a mock data plane, over both transports, and exits
// non-zero when an expected event is missing or wrong. It needs no network or credentials.
// --live runs the same check with the events sent to RUDDERSTACK_DATA_PLANE_URL, and is
// skipped without credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/ruddermcpgo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-server-mcpgo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	transport := fs.String("transport", "stdio", "stdio or http")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address for --transport=http")
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, func(sink *harness.Sink) error { return runCheck(sink, stderr) })
	}
	if err := serve(*transport, *addr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// newAnalytics is the RudderStack client, from RUDDERSTACK_WRITE_KEY and
// RUDDERSTACK_DATA_PLANE_URL.
func newAnalytics() (analytics.Client, error) {
	key, url := os.Getenv("RUDDERSTACK_WRITE_KEY"), os.Getenv("RUDDERSTACK_DATA_PLANE_URL")
	if key == "" || url == "" {
		return nil, errors.New("set RUDDERSTACK_WRITE_KEY and RUDDERSTACK_DATA_PLANE_URL")
	}
	return analytics.NewWithConfig(key, analytics.Config{DataPlaneUrl: url, NoProxySupport: true, Interval: 200 * time.Millisecond})
}

// newServer builds the workspace server and tracks it.
func newServer(client analytics.Client, transport string) (*server.MCPServer, error) {
	s := server.NewMCPServer("workspace-mcp", "1.4.0", server.WithToolCapabilities(true), server.WithRecovery())
	s.AddTool(mcp.NewTool("list_destinations", mcp.WithDescription("Lists the workspace's destinations."),
		mcp.WithString("workspace_id", mcp.Required())),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(`{"destinations":["warehouse","crm"]}`), nil
		})
	s.AddTool(mcp.NewTool("create_audience", mcp.WithDescription("Creates an audience."),
		mcp.WithString("name", mcp.Required())),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(`{"id":"aud_1","type":"computed"}`), nil
		})
	err := ruddermcpgo.WrapServer(s, ruddermcpgo.Options{
		Analytics: client,
		Transport: transport,
		EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
			"create_audience": {
				Event: "audience_created",
				Properties: func(_ map[string]any, result any) map[string]any {
					out, _ := result.(map[string]any)
					return map[string]any{"audience_type": out["type"]}
				},
			},
		}},
		RequestContext:     rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
		CapabilityRequests: &rudderstackai.CapabilityRequestOptions{Capabilities: []string{"bulk_export"}},
	})
	return s, err
}

func serve(transport, addr string) error {
	client, err := newAnalytics()
	if err != nil {
		return err
	}
	// Close flushes queued events.
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch transport {
	case "stdio":
		s, err := newServer(client, rudderstackai.TransportStdio)
		if err != nil {
			return err
		}
		// Returns when the client closes stdin.
		return server.NewStdioServer(s).Listen(ctx, os.Stdin, os.Stdout)
	case "http":
		s, err := newServer(client, "")
		if err != nil {
			return err
		}
		srv := &http.Server{Addr: addr, Handler: server.NewStreamableHTTPServer(s), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = srv.Shutdown(context.Background())
		}()
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q", transport)
	}
}
