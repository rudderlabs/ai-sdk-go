package ruddermcpgo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	sessionUUID  = "6f1c2a8e-3b4d-4c5e-9f60-718293a4b5c6"
	sessionEmail = "jane.doe@example.com"
)

// customIDs is a SessionIdManager issuing IDs that are not mcp-go's own format.
type customIDs struct{}

func (customIDs) Generate() string               { return "custom-session-1" }
func (customIDs) Validate(string) (bool, error)  { return false, nil }
func (customIDs) Terminate(string) (bool, error) { return false, nil }

func sessionIDs(r *recorder, event string) []any {
	var ids []any
	for _, m := range r.named(event) {
		ids = append(ids, m.Properties["mcp_session_id"])
	}
	return ids
}

func postTool(t *testing.T, url, header string) {
	t.Helper()
	body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"},
		"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "c", "version": "1"}},
	}})
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if header != "" {
		req.Header.Set("Mcp-Session-Id", header)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatalf("HTTP %d", res.StatusCode)
	}
}

func TestMCPSessionIDStatelessHeader(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"email", sessionEmail, ""},
		{"uuid", sessionUUID, sessionUUID},
		{"none", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{})
			ts := httptest.NewServer(server.NewStreamableHTTPServer(s, server.WithStateLess(true)))
			defer ts.Close()
			postTool(t, ts.URL, tc.header)
			postTool(t, ts.URL, tc.header)
			calls := r.named("mcp_tool_called")
			if len(calls) != 2 {
				t.Fatalf("events = %v", r.names())
			}
			for i, c := range calls {
				got, present := c.Properties["mcp_session_id"]
				if tc.want == "" && present || tc.want != "" && got != tc.want {
					t.Errorf("call %d: mcp_session_id = %v (present %v), want %q", i, got, present, tc.want)
				}
				// A rejected header still scopes call positions in-process.
				if idx, scoped := c.Properties["call_index"]; scoped != (tc.header != "") || scoped && idx != i {
					t.Errorf("call %d: call_index = %v (present %v), want %d scoped=%v", i, idx, scoped, i, tc.header != "")
				}
			}
			seen := r.named("mcp_client_seen")
			if len(seen) == 0 {
				t.Fatalf("no mcp_client_seen: %v", r.names())
			}
			if got, present := seen[0].Properties["mcp_session_id"]; tc.want == "" && present || tc.want != "" && got != tc.want {
				t.Errorf("mcp_client_seen mcp_session_id = %v, want %q", got, tc.want)
			}
		})
	}
}

// Clients echo the issued ID after initialize: the default and stateful managers issue
// mcp-session-<uuid>, which is sent on every event.
func TestMCPSessionIDIssuedByManager(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []server.StreamableHTTPOption
	}{
		{"default", nil},
		{"stateful", []server.StreamableHTTPOption{server.WithStateful(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{})
			cs := connectHTTP(t, s, "2025-06-18", nil, tc.opts...)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
			id := cs.ID()
			if !strings.HasPrefix(id, "mcp-session-") {
				t.Fatalf("session ID = %q", id)
			}
			for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
				ids := sessionIDs(r, event)
				if len(ids) == 0 {
					t.Fatalf("no %s: %v", event, r.names())
				}
				for i, got := range ids {
					if got != id {
						t.Errorf("%s %d: mcp_session_id = %v, want %q", event, i, got, id)
					}
				}
			}
		})
	}
}

// Known limit: the SDK cannot see which manager validated the header, so an ID in another
// format is sent where the transport assigned it (initialize) and not on later requests.
func TestMCPSessionIDCustomManagerKnownLimit(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	cs := connectHTTP(t, s, "2025-06-18", nil, server.WithSessionIdManager(customIDs{}))
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if cs.ID() != "custom-session-1" {
		t.Fatalf("session ID = %q", cs.ID())
	}
	if ids := sessionIDs(r, "mcp_client_seen"); len(ids) != 1 || ids[0] != "custom-session-1" {
		t.Errorf("mcp_client_seen session IDs = %v, want the issued one", ids)
	}
	if ids := sessionIDs(r, "mcp_tool_called"); len(ids) != 1 || ids[0] != nil {
		t.Errorf("mcp_tool_called session IDs = %v, want absent", ids)
	}
}

type headerTransport struct{ header string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Mcp-Session-Id", h.header)
	return http.DefaultTransport.RoundTrip(req)
}

// The SSE server issues its own ID; a client header never replaces it.
func TestMCPSessionIDSSEIssuedBeatsHeader(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	ts := httptest.NewServer(server.NewSSEServer(s))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := sdk.NewClient(testClient, nil).Connect(ctx,
		&sdk.SSEClientTransport{Endpoint: ts.URL + "/sse", HTTPClient: &http.Client{Transport: headerTransport{sessionEmail}}},
		&sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	ids := sessionIDs(r, "mcp_tool_called")
	if len(ids) != 1 || ids[0] == nil || ids[0] == sessionEmail {
		t.Fatalf("mcp_tool_called session IDs = %v, want the server's own ID", ids)
	}
	if id, _ := ids[0].(string); len(id) != 36 {
		t.Errorf("session ID = %q, want the SSE server's UUID", id)
	}
}
