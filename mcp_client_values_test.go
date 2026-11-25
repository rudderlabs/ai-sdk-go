package rudderstackai

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// Client-supplied IDs and client details through the go-sdk wrapper
// (spec/fixtures/mcp-traceparent-v0.1.json, spec/fixtures/mcp-client-info-v0.1.json).

// minted reports an interaction_id the SDK made itself: a canonical UUID v4.
func minted(id any) bool {
	s, _ := id.(string)
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u.Version() == 4
}

// connectAs connects a real go-sdk client that initializes with client.
func connectAs(t *testing.T, server *mcp.Server, client *mcp.Implementation) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(client, nil).Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

// Free text in _meta never becomes an ID; an invalid interaction_id scopes calls like an absent one.
func TestMCPClientSuppliedIDs(t *testing.T) {
	const id = "3f2c8a9e-5b1d-4c7a-9e2f-6a8b0c1d2e3f"
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{})
	cs := connect(t, s, "2025-06-18", nil)
	args := map[string]any{"workspace_id": "w"}
	callTool(t, cs, "list_destinations", args, mcp.Meta{metaInteractionID: "Jane Doe row=Saffron", metaTraceID: "Jane Doe row=Saffron"})
	callTool(t, cs, "list_destinations", args, mcp.Meta{metaInteractionID: "int_x", metaTraceID: "t-1"})
	callTool(t, cs, "list_destinations", args, mcp.Meta{metaInteractionID: id, metaTraceID: "0af7651916cd43dd8448eb211c80319c"})
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
			cs := connectAs(t, s, &mcp.Implementation{Name: "agent for jane.doe@example.com", Title: "Jane's laptop jane.doe@example.com", Version: "v1.2.3"})
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
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{ClientInfoFallback: func(context.Context, *auth.TokenInfo) (string, string) { return "fallback-agent", "2.0.0" }})
	cs := connectAs(t, s, &mcp.Implementation{Name: strings.Repeat("a", 32_001), Version: "latest"})
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
		if p := r.props(t, event); p["client_name"] != "fallback-agent" || p["client_version"] != "2.0.0" {
			t.Errorf("%s: client_name %v, client_version %v", event, p["client_name"], p["client_version"])
		}
	}
}

// Declared-event and subscription client_* properties follow the same rules, never the app's Redact;
// a rule's client_name and client_version are reserved.
func TestMCPAppClientPropertiesChecked(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{
		MCPEvents: true,
		Redact:    func(string) (string, error) { return "app-redacted", nil },
		EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "destinations_listed", Properties: func(map[string]any, any) map[string]any {
			return map[string]any{"client_name": "rule-agent", "client_title": "\tJane Doe jane.doe@example.com ", "client_version": "9.9.9"}
		}}}},
	})
	// No name or version at initialize: the rule's still never reach the event.
	cs := connectAs(t, s, &mcp.Implementation{})
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	p := r.props(t, "destinations_listed")
	_, name := p["client_name"]
	_, version := p["client_version"]
	if name || version || p["client_title"] != "Jane Doe [email]" {
		t.Errorf("declared event = %v", p)
	}
	for _, tc := range []struct{ name, want string }{{"agent for jane.doe@example.com", "agent for [email]"}, {strings.Repeat("a", 32_001), ""}} {
		r.reset()
		TrackMCPEventSubscription(context.Background(), s, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "sub-1", EventName: "comment.created", ClientName: tc.name})
		got, ok := r.props(t, "mcp_event_subscription_changed")["client_name"]
		if (tc.want == "" && ok) || (tc.want != "" && got != tc.want) {
			t.Errorf("subscription client_name = %v, want %q", got, tc.want)
		}
	}
}

// Every client source is checked where it is read: a masked name and a version that is not
// version-shaped never reach an event raw.
func TestMCPClientSourcesChecked(t *testing.T) {
	const name, version = "agent for jane.doe@example.com", "v1.2.3"
	given := func(context.Context, *auth.TokenInfo) (string, string) { return name, version }
	for _, tc := range []struct {
		name string
		opts Options
		req  mcpbridge.Request
		want string
	}{
		{"meta_client_info", Options{}, mcpbridge.Request{Meta: map[string]any{metaClientInfo: map[string]any{"name": name, "version": version}}}, "agent for [email]"},
		{"client_info", Options{ClientInfo: given}, mcpbridge.Request{}, "agent for [email]"},
		{"client_info_fallback", Options{ClientInfoFallback: given}, mcpbridge.Request{}, "agent for [email]"},
		{"user_agent", Options{}, mcpbridge.Request{HTTP: true, UserAgent: "Cursor/10.0.0.12"}, "Cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, tc.opts)
			w, _ := instances.get(s)
			tc.req.Ctx = context.Background()
			rc := w.begin(tc.req)
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
			p := r.props(t, "mcp_tool_called")
			if _, ok := p["client_version"]; ok || p["client_name"] != tc.want {
				t.Errorf("client_name %v, client_version %v, want %q and none", p["client_name"], p["client_version"], tc.want)
			}
		})
	}
}

// The client an initialize result reports is checked before it is remembered for later requests.
func TestMCPInitializedClientCheckedBeforeRemembered(t *testing.T) {
	for _, tc := range []struct {
		name       string
		initialize *mcpbridge.Client // what the initialize request carried
		want       string
	}{
		{"request_without_client_info", nil, "agent for [email]"},
		// The name's cut exposes a phone and the version is not version-shaped: nothing is left.
		{"client_info_failing_every_check", &mcpbridge.Client{Name: strings.Repeat("x", 51) + " 415-555-0100-9", Version: "v1.2.3"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) {
				return Identity{UserID: "u", OAuthClientID: "app"}, nil
			}})
			w, _ := instances.get(s)
			reported := &mcpbridge.Client{Name: "agent for jane.doe@example.com", Version: "v1.2.3"}
			if tc.initialize != nil {
				reported = tc.initialize
			}
			w.begin(mcpbridge.Request{Ctx: context.Background(), Initialize: true, InitializeClient: tc.initialize}).Initialized(reported, nil, "2025-06-18")
			rc := w.begin(mcpbridge.Request{Ctx: context.Background()})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
			p := r.props(t, "mcp_tool_called")
			got, named := p["client_name"]
			if _, ok := p["client_version"]; ok || (tc.want == "" && named) || (tc.want != "" && got != tc.want) {
				t.Errorf("client_name %v, client_version %v, want %q and none", got, p["client_version"], tc.want)
			}
		})
	}
}
