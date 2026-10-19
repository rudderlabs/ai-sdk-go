// Command mcp-stateless-http is a stateless streamable HTTP MCP server on protocol 2026-07-28,
// tracked with rudderstackai. Its delete_audience tool asks the user to confirm through an
// input_required result, which the client answers in a retry of the same call.
//
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/mcp-stateless-http
//
// --check runs it end to end against a mock data plane; --live sends the check's events to
// RUDDERSTACK_DATA_PLANE_URL and is skipped without credentials.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-stateless-http", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, func(sink *harness.Sink) error { return runCheck(sink, stderr) })
	}
	if err := serve(*addr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type deleteIn struct {
	AudienceID string `json:"audience_id"`
}

// newHandler builds the server, tracks it and serves it statelessly: each request carries its
// protocol version and client info in _meta, and there is no session.
func newHandler(client analytics.Client) (http.Handler, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "audience-admin-mcp", Version: "2.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_audience", Description: "Deletes an audience after the user confirms."},
		func(_ context.Context, req *mcp.CallToolRequest, in deleteIn) (*mcp.CallToolResult, any, error) {
			answer, _ := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
			if answer == nil {
				// First leg: ask. requestState is opaque to the client and unique per request.
				return &mcp.CallToolResult{
					InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Message: "Delete audience " + in.AudienceID + "?"}},
					RequestState:  "delete:" + rand.Text(),
				}, nil, nil
			}
			text := "kept"
			if answer.Action == "accept" {
				text = "deleted"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{Analytics: client}); err != nil {
		return nil, err
	}
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}), nil
}

func serve(addr string) error {
	client, err := harness.EnvClient()
	if err != nil {
		return err
	}
	// Close flushes queued events.
	defer client.Close()
	h, err := newHandler(client)
	if err != nil {
		return err
	}
	return harness.Serve(addr, h)
}
