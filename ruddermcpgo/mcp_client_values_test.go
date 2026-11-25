package ruddermcpgo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Client-supplied IDs and client details through the mcp-go wrapper, as for the go-sdk
// (spec/fixtures/mcp-traceparent-v0.1.json, spec/fixtures/mcp-client-info-v0.1.json).

func minted(id any) bool {
	s, _ := id.(string)
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u.Version() == 4
}

// connectHTTPAs serves s over streamable HTTP to a go-sdk client that initializes with client.
func connectHTTPAs(t *testing.T, s *server.MCPServer, client *sdk.Implementation, httpClient *http.Client) *sdk.ClientSession {
	t.Helper()
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)
	cs, err := sdk.NewClient(client, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Free text in _meta never becomes an ID; an invalid interaction_id scopes calls like an absent one.
func TestMCPClientSuppliedIDs(t *testing.T) {
	const id = "3f2c8a9e-5b1d-4c7a-9e2f-6a8b0c1d2e3f"
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", nil)
	args := map[string]any{"workspace_id": "w"}
	callTool(t, cs, "list_destinations", args, sdk.Meta{metaInteractionID: "Jane Doe row=Saffron", "com.rudderstack/trace_id": "Jane Doe row=Saffron"})
	callTool(t, cs, "list_destinations", args, sdk.Meta{metaInteractionID: "int_x", "com.rudderstack/trace_id": "t-1"})
	callTool(t, cs, "list_destinations", args, sdk.Meta{metaInteractionID: id, "com.rudderstack/trace_id": "0af7651916cd43dd8448eb211c80319c"})
	callTool(t, cs, "list_destinations", args, nil)
	got := r.named("mcp_tool_called")
	if len(got) != 4 {
		t.Fatalf("%d events", len(got))
	}
	// The connection counts the invalid and absent ones; the valid ID has its own count.
	for i, index := range []int{0, 1, 0, 2} {
		p := got[i].Properties
		if p["call_index"] != index {
			t.Errorf("call %d: call_index = %v, want %d", i, p["call_index"], index)
		}
		if i == 2 {
			if p["interaction_id"] != id || p["trace_id"] != "0af7651916cd43dd8448eb211c80319c" {
				t.Errorf("valid ids: %v", p)
			}
			continue
		}
		if _, ok := p["trace_id"]; ok || !minted(p["interaction_id"]) {
			t.Errorf("call %d: interaction_id %v, trace_id %v", i, p["interaction_id"], p["trace_id"])
		}
	}
	if all := mustJSON(r.all()); strings.Contains(all, "Saffron") || strings.Contains(all, "int_x") || strings.Contains(all, `"t-1"`) {
		t.Errorf("client text sent: %s", all)
	}
}

// client_name and client_title are masked with the built-in rules only; client_version must be version-shaped.
func TestMCPClientInfoChecked(t *testing.T) {
	for name, opts := range map[string]Options{
		"redact_hook":        {Redact: func(string) (string, error) { return "app-redacted", nil }},
		"redaction_disabled": {DisableRedaction: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, opts)
			cs := connectHTTPAs(t, s, &sdk.Implementation{Name: "agent for jane.doe@example.com", Title: "Jane's laptop jane.doe@example.com", Version: "v1.2.3"}, nil)
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
			for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
				p := r.props(t, event)
				if _, ok := p["client_version"]; ok || p["client_name"] != "agent for [email]" {
					t.Errorf("%s: client_name %v, client_version %v", event, p["client_name"], p["client_version"])
				}
			}
			if p := r.props(t, "mcp_client_seen"); p["client_title"] != "Jane's laptop [email]" {
				t.Errorf("client_title = %v", p["client_title"])
			}
		})
	}
}

// A clientInfo whose name and version both fail counts as not given: the next source supplies the client.
func TestMCPClientInfoFailingSourceIsSkipped(t *testing.T) {
	for name, opts := range map[string]Options{
		"fallback":   {ClientInfoFallback: func(context.Context) (string, string) { return "fallback-agent", "2.0.0" }},
		"user_agent": {},
	} {
		t.Run(name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, opts)
			cs := connectHTTPAs(t, s, &sdk.Implementation{Name: strings.Repeat("a", 32_001), Version: "latest"}, &http.Client{Transport: userAgentTransport{}})
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
			want := [2]string{"Cursor", "1.7.4"}
			if name == "fallback" {
				want = [2]string{"fallback-agent", "2.0.0"}
			}
			for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
				if p := r.props(t, event); p["client_name"] != want[0] || p["client_version"] != want[1] {
					t.Errorf("%s: client_name %v, client_version %v, want %q", event, p["client_name"], p["client_version"], want)
				}
			}
		})
	}
}

// Declared-event and subscription client_* properties follow the same rules, never the app's Redact.
func TestMCPAppClientPropertiesChecked(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{
		MCPEvents: true,
		Redact:    func(string) (string, error) { return "app-redacted", nil },
		EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"list_destinations": {Event: "destinations_listed", Properties: func(map[string]any, any) map[string]any {
			return map[string]any{"client_name": "agent for jane.doe@example.com", "client_title": "\tJane Doe ", "client_version": "v1.2.3"}
		}}}},
	})
	// No name or version at initialize, so the base leaves the declared values in place.
	cs := connectHTTPAs(t, s, &sdk.Implementation{}, &http.Client{Transport: blankUserAgent{}})
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	p := r.props(t, "destinations_listed")
	if _, ok := p["client_version"]; ok || p["client_name"] != "agent for [email]" || p["client_title"] != "Jane Doe" {
		t.Errorf("declared event = %v", p)
	}
	for _, tc := range []struct{ name, want string }{{"agent for jane.doe@example.com", "agent for [email]"}, {strings.Repeat("a", 32_001), ""}} {
		before := len(r.named("mcp_event_subscription_changed"))
		TrackMCPEventSubscription(context.Background(), s, rudderstackai.SubscriptionChange{Action: rudderstackai.SubscriptionCreated, SubscriptionID: "sub-1", EventName: "comment.created", ClientName: tc.name})
		events := r.named("mcp_event_subscription_changed")
		if len(events) != before+1 {
			t.Fatalf("%d subscription events", len(events)-before)
		}
		got, ok := events[before].Properties["client_name"]
		if (tc.want == "" && ok) || (tc.want != "" && got != tc.want) {
			t.Errorf("subscription client_name = %v, want %q", got, tc.want)
		}
	}
}

// blankUserAgent sends no User-Agent, so no fallback client applies.
type blankUserAgent struct{}

func (blankUserAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", "")
	return http.DefaultTransport.RoundTrip(req)
}
