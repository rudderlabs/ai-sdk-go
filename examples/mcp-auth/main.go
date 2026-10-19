// Command mcp-auth is a streamable HTTP MCP server behind a bearer token check, tracked with
// rudderstackai. Its Identify func maps the verified token to the user (the OAuth subject),
// the workspace (account_id) and the OAuth client; the token itself is never sent.
//
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... MCP_DEMO_TOKEN=... go run ./examples/mcp-auth
//
// --check runs it end to end against a mock data plane; --live sends the check's events to
// RUDDERSTACK_DATA_PLANE_URL and is skipped without credentials.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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
	fs := flag.NewFlagSet("mcp-auth", flag.ContinueOnError)
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

// grant is what an authorization server would have issued for a token.
type grant struct {
	token string
	info  auth.TokenInfo
}

// verifier stands in for your OAuth token verification: it accepts only the given grants.
func verifier(grants []grant) auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		for _, g := range grants {
			if subtle.ConstantTimeCompare([]byte(token), []byte(g.token)) == 1 {
				info := g.info
				info.Expiration = time.Now().Add(time.Hour)
				return &info, nil
			}
		}
		return nil, auth.ErrInvalidToken
	}
}

// identify maps the verified token to who the request is for. userId and token_scopes
// default to the token's UserID and Scopes.
func identify(_ context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
	if tok == nil {
		return rudderstackai.Identity{}, nil
	}
	workspace, _ := tok.Extra["workspace_id"].(string)
	clientID, _ := tok.Extra["client_id"].(string)
	return rudderstackai.Identity{AccountID: workspace, OAuthClientID: clientID}, nil
}

type listIn struct{}

type listOut struct {
	Sources []string `json:"sources"`
}

// newHandler builds the server, tracks it and serves it behind the bearer check.
func newHandler(client analytics.Client, grants []grant) (http.Handler, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "workspace-auth-mcp", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "list_sources", Description: "Lists the workspace's sources."},
		func(context.Context, *mcp.CallToolRequest, listIn) (*mcp.CallToolResult, listOut, error) {
			return nil, listOut{Sources: []string{"web", "ios"}}, nil
		})
	if err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{Analytics: client, Identify: identify}); err != nil {
		return nil, err
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	return auth.RequireBearerToken(verifier(grants), nil)(h), nil
}

func serve(addr string) error {
	token := os.Getenv("MCP_DEMO_TOKEN")
	if token == "" {
		return errors.New("set MCP_DEMO_TOKEN, the bearer token this demo accepts")
	}
	client, err := harness.EnvClient()
	if err != nil {
		return err
	}
	// Close flushes queued events.
	defer client.Close()
	h, err := newHandler(client, []grant{{token, auth.TokenInfo{
		UserID: "3f2b8c1e-7a4d-4e9b-b6c5-0d1e2f3a4b5c", Scopes: []string{"sources:read"},
		Extra: map[string]any{"workspace_id": "ws_demo", "client_id": "mcpc_demo"},
	}}})
	if err != nil {
		return err
	}
	return harness.Serve(addr, h)
}
