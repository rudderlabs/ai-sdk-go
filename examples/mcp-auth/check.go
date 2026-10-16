package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// runs numbers the checks in this process: state is kept per server name for the whole
// process, so a second run (--check, then --live, in one test binary) uses fresh client names.
var runs atomic.Int64

func runSuffix() string {
	if n := runs.Add(1); n > 1 {
		return "-" + strconv.FormatInt(n, 10)
	}
	return ""
}

// suffix is this run's client name suffix.
var suffix string

// checkGrants are two users of two workspaces, each through a different OAuth client.
var checkGrants = []grant{
	{"tok_check_admin", auth.TokenInfo{UserID: "oauth|user_admin", Scopes: []string{"sources:read", "sources:write"},
		Extra: map[string]any{"workspace_id": "ws_acme", "client_id": "mcpc_desktop"}}},
	{"tok_check_viewer", auth.TokenInfo{UserID: "oauth|user_viewer", Scopes: []string{"sources:read"},
		Extra: map[string]any{"workspace_id": "ws_globex", "client_id": "mcpc_ide"}}},
}

// runCheck serves one long-lived server to both users, then sends a request without a token.
// Each user has its own client name: state is kept per server name.
func runCheck(sink *harness.Sink, stderr io.Writer) error {
	suffix = runSuffix()
	client, err := sink.NewClient()
	if err != nil {
		return err
	}
	h, err := newHandler(client, checkGrants)
	if err != nil {
		return err
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	sessions := map[string]string{}
	for _, g := range checkGrants {
		id, err := agent(ts.URL, g.token, clientName(g))
		if err != nil {
			return fmt.Errorf("%s: %w", g.info.UserID, err)
		}
		sessions[g.info.UserID] = id
	}
	res, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("request without a token: status %d, want 401", res.StatusCode)
	}
	// Close flushes every queued event before the asserts.
	if err := client.Close(); err != nil {
		return err
	}
	all := sink.Messages()
	for _, g := range checkGrants {
		var msgs []dataplane.Message
		for _, m := range all {
			if m.UserID == g.info.UserID {
				msgs = append(msgs, m)
			}
		}
		if err := expectEvents(msgs, g, sessions[g.info.UserID]); err != nil {
			return fmt.Errorf("%s: %w", g.info.UserID, err)
		}
		fmt.Fprintf(stderr, "ok  %s: %d events\n", g.info.UserID, len(msgs))
	}
	if len(all) != 6 {
		return fmt.Errorf("got %d events, want 6 (none for the request without a token)", len(all))
	}
	b, _ := json.Marshal(all)
	for _, g := range checkGrants {
		if strings.Contains(string(b), g.token) {
			return fmt.Errorf("a bearer token reached the events")
		}
	}
	return nil
}

func clientName(g grant) string { return "agent-" + g.info.Extra["client_id"].(string) + suffix }

type bearer string

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(req)
}

// agent connects with token and calls list_sources twice, the second time as a retry. It
// returns the session ID.
func agent(url, token, name string) (string, error) {
	ctx := context.Background()
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer(token)}, DisableStandaloneSSE: true}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1.0.0"}, nil).
		Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		return "", err
	}
	defer cs.Close()
	for range 2 {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sources", Arguments: map[string]any{}}); err != nil {
			return "", err
		}
	}
	return cs.ID(), nil
}

// expectEvents checks one user's events (those whose userId is the token's subject) field by
// field.
func expectEvents(msgs []dataplane.Message, g grant, sessionID string) error {
	byEvent := harness.ByEvent(msgs)
	if err := harness.Counts(byEvent, map[string]int{"mcp_client_seen": 1, "mcp_tool_called": 2}); err != nil {
		return err
	}
	account := g.info.Extra["workspace_id"]
	if err := harness.Fields("mcp_client_seen", byEvent["mcp_client_seen"][0].Properties, map[string]any{
		"server_name": "workspace-auth-mcp", "client_name": clientName(g), "transport": "streamable_http",
		"protocol_version": "2025-06-18", "mcp_session_id": sessionID, "auth_method": "oauth",
		"oauth_client_id": g.info.Extra["client_id"], "token_scopes": g.info.Scopes, "account_id": account,
	}); err != nil {
		return err
	}
	for i := range 2 {
		if err := harness.Fields("mcp_tool_called", harness.Find(byEvent["mcp_tool_called"], "call_index", i), map[string]any{
			"tool_name": "list_sources", "tool_status": "success", "account_id": account, "mcp_session_id": sessionID,
			"client_name": clientName(g), "call_index": i, "retry_count": i,
			// Only mcp_client_seen carries the OAuth client and scopes.
			"oauth_client_id": harness.Absent, "token_scopes": harness.Absent,
		}); err != nil {
			return err
		}
	}
	return nil
}
