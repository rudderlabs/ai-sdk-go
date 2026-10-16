// Command mcp-server is an MCP server tracked with rudderstackai, served over stdio or
// streamable HTTP.
//
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-server --transport=http
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

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-server", flag.ContinueOnError)
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

type listIn struct {
	WorkspaceID string `json:"workspace_id"`
}

type listOut struct {
	Destinations []string `json:"destinations"`
}

type audienceIn struct {
	Name string `json:"name"`
}

type audienceOut struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// newServer builds the workspace server and tracks it.
func newServer(client analytics.Client, transport string) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "list_destinations", Description: "Lists the workspace's destinations."},
		func(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
			return nil, listOut{Destinations: []string{"warehouse", "crm"}}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "create_audience", Description: "Creates an audience."},
		func(_ context.Context, _ *mcp.CallToolRequest, in audienceIn) (*mcp.CallToolResult, audienceOut, error) {
			return nil, audienceOut{ID: "aud_1", Type: "computed"}, nil
		})
	err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
		Analytics: client,
		Transport: transport,
		Identify: func(_ context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
			if tok == nil {
				return rudderstackai.Identity{}, nil
			}
			workspace, _ := tok.Extra["workspace_id"].(string)
			return rudderstackai.Identity{AccountID: workspace}, nil
		},
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
	return server, err
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
		server, err := newServer(client, rudderstackai.TransportStdio)
		if err != nil {
			return err
		}
		// Returns when the client closes stdin.
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		server, err := newServer(client, "")
		if err != nil {
			return err
		}
		srv := &http.Server{Addr: addr, Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil), ReadHeaderTimeout: 10 * time.Second}
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
