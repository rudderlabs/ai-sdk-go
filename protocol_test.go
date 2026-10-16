package rudderstackai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmServer has a tool that asks the user to confirm: through session.Elicit
// (ask_legacy), or by returning input requests (confirm), which the go-sdk turns into an
// elicitation/create request for 2025-06-18 clients and an input_required result on 2026-07-28.
func confirmServer(name string, elicitTimeout time.Duration) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "2.0.0"}, nil)
	schema := map[string]any{"type": "object", "properties": map[string]any{"audience": map[string]any{"type": "string"}}}
	s.AddTool(&mcp.Tool{Name: "confirm", InputSchema: schema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{"ok": &mcp.ElicitParams{Message: "Create the secret audience?"}},
				RequestState:  "state-1",
			}, nil
		}
		er, _ := req.Params.InputResponses["ok"].(*mcp.ElicitResult)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done: " + er.Action}}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "ask_legacy", InputSchema: schema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if elicitTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, elicitTimeout)
			defer cancel()
		}
		res, err := Elicit(ctx, req.Session, &mcp.ElicitParams{Message: "Your secret answer?"})
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no answer"}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Action}}}, nil
	})
	return s
}

func elicitingClient(action string) *mcp.ClientOptions {
	return &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: action, Content: map[string]any{"answer": "private answer"}}, nil
	}}
}

func TestElicitation2025(t *testing.T) {
	server := confirmServer("confirm-mcp", 0)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", elicitingClient("accept"))
	callTool(t, cs, "ask_legacy", map[string]any{}, mcp.Meta{metaInteractionID: "int_e"})
	callTool(t, cs, "confirm", map[string]any{}, nil)
	got := r.named("mcp_elicitation_completed")
	if len(got) != 2 {
		t.Fatalf("events = %v", r.names())
	}
	calls := r.named("mcp_tool_called")
	for i, m := range got {
		p := m.Properties
		if p["elicitation_action"] != "accept" || p["tool_name"] != calls[i].Properties["tool_name"] || p["request_id"] != calls[i].Properties["request_id"] {
			t.Errorf("elicitation %d = %v", i, p)
		}
		if _, ok := p["duration_ms"]; !ok || p["error_type"] != nil {
			t.Errorf("elicitation %d = %v", i, p)
		}
	}
	if got[0].Properties["interaction_id"] != "int_e" {
		t.Errorf("interaction_id = %v", got[0].Properties["interaction_id"])
	}
	if all := mustJSON(r.all()); strings.Contains(all, "secret") || strings.Contains(all, "private answer") {
		t.Error("question or answer leaked")
	}
}

func TestElicitationClientWithoutSupport(t *testing.T) {
	server := confirmServer("confirm-mcp", 0)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "ask_legacy", map[string]any{}, nil)
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "cancel" || p["error_type"] != "invalid_request" || p["tool_name"] != "ask_legacy" {
		t.Errorf("elicitation = %v", p)
	}
	if _, ok := p["duration_ms"]; ok {
		t.Error("duration_ms without a request")
	}
}

func TestElicitationClientError(t *testing.T) {
	server := confirmServer("confirm-mcp", 0)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return nil, errors.New("client broke")
	}})
	callTool(t, cs, "ask_legacy", map[string]any{}, nil)
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "cancel" || p["error_type"] != "api_error" {
		t.Errorf("elicitation = %v", p)
	}
}

func TestElicitationTimeout(t *testing.T) {
	server := confirmServer("confirm-mcp", 20*time.Millisecond)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", &mcp.ClientOptions{ElicitationHandler: func(ctx context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		return &mcp.ElicitResult{Action: "accept"}, nil
	}})
	callTool(t, cs, "ask_legacy", map[string]any{}, nil)
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "cancel" || p["error_type"] != "timeout" {
		t.Errorf("elicitation = %v", p)
	}
}

func TestStateless2026ClientPerRequest(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "", nil)
	if got := cs.InitializeResult().ProtocolVersion; got != protocol2026 {
		t.Fatalf("negotiated %s, want %s", got, protocol2026)
	}
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	seen := r.props(t, "mcp_client_seen")
	if seen["protocol_version"] != protocol2026 || seen["client_name"] != "test-client" || seen["client_title"] != "Test Client" {
		t.Errorf("client seen = %v", seen)
	}
	if _, ok := seen["mcp_session_id"]; ok {
		t.Error("stateless request with a session ID")
	}
	for _, m := range r.named("mcp_tool_called") {
		if m.Properties["protocol_version"] != protocol2026 || m.Properties["client_version"] != "1.2.3" {
			t.Errorf("tool call = %v", m.Properties)
		}
	}
}

func TestInputRequired2026(t *testing.T) {
	server := confirmServer("confirm-mcp", 0)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "", elicitingClient("decline"))
	res := callTool(t, cs, "confirm", map[string]any{}, mcp.Meta{metaInteractionID: "int_2026"})
	if text := res.Content[0].(*mcp.TextContent).Text; text != "done: decline" {
		t.Fatalf("result = %s", text)
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 1 {
		t.Fatalf("mcp_tool_called = %d, want 1 (the input_required leg sends none)", len(calls))
	}
	if _, ok := calls[0].Properties["retry_count"]; ok {
		t.Error("retry_count on the retry that answers the server")
	}
	if calls[0].Properties["call_index"] != 1 {
		t.Errorf("call_index = %v", calls[0].Properties["call_index"])
	}
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "decline" || p["tool_name"] != "confirm" || p["interaction_id"] != "int_2026" || p["protocol_version"] != protocol2026 {
		t.Errorf("elicitation = %v", p)
	}
	if p["request_id"] == calls[0].Properties["request_id"] || p["request_id"] == nil {
		t.Errorf("request_id = %v, want the original request's", p["request_id"])
	}
	if _, ok := p["duration_ms"]; !ok {
		t.Error("no duration_ms")
	}
}

func TestInputResponsesWithoutState(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "stateless-confirm", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "confirm", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"ok": &mcp.ElicitParams{Message: "Sure?"}}}, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
	})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "", elicitingClient("accept"))
	callTool(t, cs, "confirm", map[string]any{}, nil)
	p := r.props(t, "mcp_elicitation_completed")
	if p["elicitation_action"] != "accept" || p["request_id"] == nil {
		t.Errorf("elicitation = %v", p)
	}
}

// httpServer serves server over streamable HTTP, behind a bearer token check.
func httpServer(t *testing.T, server *mcp.Server, opts *mcp.StreamableHTTPOptions) string {
	t.Helper()
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != "tok_secret" {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: "oauth|8812", Scopes: []string{"workspace:read"}, Expiration: time.Now().Add(time.Hour),
			Extra: map[string]any{"workspace": "workspace_acme", "client_id": "mcpc_7Hq2"}}, nil
	}
	h := auth.RequireBearerToken(verify, nil)(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

type bearer struct{}

func (bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer tok_secret")
	return http.DefaultTransport.RoundTrip(req)
}

func connectHTTP(t *testing.T, url, protocol string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{}}, DisableStandaloneSSE: true}
	cs, err := mcp.NewClient(testClientInfo, nil).Connect(context.Background(), transport, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func identifyFromToken(_ context.Context, tok *auth.TokenInfo) (Identity, error) {
	if tok == nil {
		return Identity{}, nil
	}
	return Identity{AccountID: str(tok.Extra["workspace"]), OAuthClientID: str(tok.Extra["client_id"]), Country: "DE"}, nil
}

func TestStreamableHTTPSessions(t *testing.T) {
	server := workspaceServer("http-mcp", nil)
	r := wrap(t, server, Options{Identify: identifyFromToken})
	cs := connectHTTP(t, httpServer(t, server, nil), "2025-06-18")
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	seen := r.props(t, "mcp_client_seen")
	want := map[string]any{
		"transport": "streamable_http", "auth_method": "oauth", "oauth_client_id": "mcpc_7Hq2",
		"account_id": "workspace_acme", "country": "DE", "protocol_version": "2025-06-18", "mcp_session_id": cs.ID(),
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %v, want %v", k, seen[k], v)
		}
	}
	if !slices.Equal(seen["token_scopes"].([]string), []string{"workspace:read"}) {
		t.Errorf("token_scopes = %v", seen["token_scopes"])
	}
	calls := r.named("mcp_tool_called")
	for i, m := range calls {
		if m.UserId != "oauth|8812" || m.Properties["mcp_session_id"] != cs.ID() || m.Properties["call_index"] != i || m.Properties["retry_count"] != i {
			t.Errorf("call %d = %v / %s", i, m.Properties, m.UserId)
		}
	}
	if strings.Contains(mustJSON(r.all()), "tok_secret") {
		t.Error("token leaked")
	}
}

func TestStreamableHTTPStateless2026(t *testing.T) {
	server := workspaceServer("stateless-mcp", nil)
	r := wrap(t, server, Options{Identify: identifyFromToken})
	cs := connectHTTP(t, httpServer(t, server, &mcp.StreamableHTTPOptions{Stateless: true}), "")
	for range 2 {
		callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, mcp.Meta{metaInteractionID: "int_h"})
	}
	seen := r.props(t, "mcp_client_seen")
	if seen["protocol_version"] != protocol2026 || seen["transport"] != "streamable_http" || seen["client_name"] != "test-client" {
		t.Errorf("client seen = %v", seen)
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 2 || calls[1].Properties["call_index"] != 1 || calls[1].Properties["retry_count"] != 1 {
		t.Errorf("calls = %v", mustJSON(calls))
	}
	if _, ok := calls[0].Properties["mcp_session_id"]; ok {
		t.Error("stateless call with a session ID")
	}
}

// A latest-protocol client probes a stateful server with server/discover, learns it serves only
// legacy versions and falls back to initialize. Only the handshake that succeeds is reported.
func TestStatefulServerDiscoverProbeFallsBack(t *testing.T) {
	server := workspaceServer("stateful-mcp", nil)
	r := wrap(t, server, Options{Identify: identifyFromToken})
	cs := connectHTTP(t, httpServer(t, server, nil), "")
	if got := cs.InitializeResult().ProtocolVersion; got != legacyProtocol {
		t.Fatalf("negotiated %s, want %s", got, legacyProtocol)
	}
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	seen := r.named("mcp_client_seen")
	if len(seen) != 1 {
		t.Fatalf("mcp_client_seen = %d, want 1", len(seen))
	}
	call := r.props(t, "mcp_tool_called")
	p := seen[0].Properties
	if p["protocol_version"] != legacyProtocol || p["mcp_session_id"] != cs.ID() || call["mcp_session_id"] != cs.ID() || call["protocol_version"] != legacyProtocol {
		t.Errorf("client seen = %v, tool call = %v, session %s", p, call, cs.ID())
	}
	if len(r.all()) != 2 {
		t.Errorf("events = %v, want only the handshake's and the call's", r.names())
	}
}

// On 2026-07-28 the client is reported after a successful request, never a failed one.
func TestClientSeen2026OnlyAfterSuccess(t *testing.T) {
	server := workspaceServer("success-mcp", nil)
	// A window this short reports the client at every request that may report it.
	r := wrap(t, server, Options{ClientSeenWindow: time.Nanosecond})
	cs := connect(t, server, "", nil)
	r.reset()
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "missing_tool"}); err == nil {
		t.Fatal("want a protocol error")
	}
	if got := len(r.named("mcp_client_seen")); got != 0 {
		t.Fatalf("mcp_client_seen after a failed request = %d", got)
	}
	time.Sleep(time.Millisecond)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if got := len(r.named("mcp_client_seen")); got != 1 {
		t.Errorf("mcp_client_seen after a successful request = %d", got)
	}
}
