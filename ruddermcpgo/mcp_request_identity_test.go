package ruddermcpgo

import (
	"context"
	"net/http"
	"reflect"

	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func stringProperty(p map[string]any, key string) string {
	value, _ := p[key].(string)
	return value
}

func TestRequestOAuthEveryEvent(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	opts := fullOptions()
	opts.MCPEvents = true
	opts.Identify = func(context.Context) (rudderstackai.Identity, error) {
		return rudderstackai.Identity{UserID: "user", OAuthClientID: "app", AuthMethod: "oauth", TokenScopes: []string{"private"}, Country: "IN"}, nil
	}
	r := wrap(t, s, opts)
	cs := connectStdio(t, s, "2025-06-18", &sdk.ClientOptions{ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "accept"}, nil
	}})
	exercise(t, cs)
	TrackMCPEventSubscription(context.Background(), s, rudderstackai.SubscriptionChange{Action: rudderstackai.SubscriptionCreated, SubscriptionID: "sub", EventName: "update"})
	send := WrapMCPEventSender(s, func(context.Context, rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
		return rudderstackai.DeliveryOutcome{HTTPStatus: 200}, nil
	})
	if _, err := send(context.Background(), rudderstackai.EventDelivery{SubscriptionID: "sub", EventID: "event", EventName: "update"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mcp_tool_called", "mcp_resource_read", "mcp_prompt_used", "mcp_capability_requested", "audience_created"} {
		events := r.named(name)
		if len(events) == 0 {
			t.Fatalf("missing %s", name)
		}
		for _, event := range events {
			p := event.Properties
			if p["oauth_client_id"] != "app" || p["auth_method"] != "oauth" {
				t.Errorf("%s auth = %v", name, p)
			}
			for _, key := range []string{"token_scopes", "country"} {
				if _, ok := p[key]; ok {
					t.Errorf("%s has %s", name, key)
				}
			}
		}
	}
	for _, name := range []string{"mcp_event_subscription_changed", "mcp_event_delivered"} {
		p := r.props(t, name)
		for _, key := range []string{"oauth_client_id", "auth_method"} {
			if _, ok := p[key]; ok {
				t.Errorf("%s has %s", name, key)
			}
		}
	}
}

func TestRememberedClientPerRequestServer(t *testing.T) {
	for _, scenario := range []string{"same", "user", "oauth", "anonymous", "empty", "oauth_only", "user_only", "expired", "resolver", "meta", "latest", "failed", "server"} {
		t.Run(scenario, func(t *testing.T) {
			id := rudderstackai.Identity{UserID: "user", OAuthClientID: "app", AuthMethod: "api_key"}
			if scenario == "anonymous" {
				id.UserID, id.AnonymousID = "", "anon"
			}
			if scenario == "empty" || scenario == "oauth_only" {
				id.UserID = ""
			}
			if scenario == "empty" || scenario == "user_only" {
				id.OAuthClientID = ""
			}
			opts := Options{Identify: func(context.Context) (rudderstackai.Identity, error) { return id, nil }}
			if scenario == "expired" {
				opts.ClientSeenWindow = time.Millisecond
			}
			initialize := func(name string) {
				s := workspaceServer(t.Name(), nil)
				wrap(t, s, opts)
				method := "initialize"
				if scenario == "failed" {
					method = "initialize-invalid"
				}
				response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": name, "version": "1"}}})))
				_, failed := response.(mcp.JSONRPCError)
				if failed && scenario != "failed" {
					t.Fatal(response)
				}
			}
			initialize("remembered")
			want := "remembered"
			switch scenario {
			case "user":
				id.UserID = "other"
				want = ""
			case "oauth":
				id.OAuthClientID = "other"
				want = ""
			case "empty", "failed", "server":
				want = ""
			case "expired":
				time.Sleep(5 * time.Millisecond)
				want = ""
			case "resolver":
				opts.ClientInfo = func(context.Context) (string, string) { return "resolved", "1" }
				want = "resolved"
			case "meta":
				want = "metadata"
			case "latest":
				initialize("latest")
				want = "latest"
			}
			name := t.Name()
			if scenario == "server" {
				name += "-other"
			}
			s := workspaceServer(name, nil)
			r := wrap(t, s, opts)
			meta := map[string]any{}
			if scenario == "meta" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "metadata", "version": "1"}
			}
			response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}, "_meta": meta}})))
			if _, failed := response.(mcp.JSONRPCError); failed {
				t.Fatal(response)
			}
			p := r.props(t, "mcp_tool_called")
			if stringProperty(p, "client_name") != want || (want != "" && p["client_version"] != "1") {
				t.Errorf("client = %v, want %q", p, want)
			}
			if stringProperty(p, "oauth_client_id") != id.OAuthClientID || p["auth_method"] != "api_key" {
				t.Errorf("auth = %v", p)
			}
			if want == "" {
				for _, key := range []string{"client_name", "client_version"} {
					if _, ok := p[key]; ok {
						t.Errorf("unknown client has %s", key)
					}
				}
			}
			if id.OAuthClientID == "" {
				if _, ok := p["oauth_client_id"]; ok {
					t.Error("unknown OAuth client was sent")
				}
			}

			for _, key := range []string{"mcp_session_id", "protocol_version", "token_scopes", "country"} {
				if _, ok := p[key]; ok {
					t.Errorf("unexpected %s: %v", key, p)
				}
			}
			if scenario != "meta" && len(r.named("mcp_client_seen")) != 0 {
				t.Error("remembered client emitted mcp_client_seen")
			}
		})
	}
}

func TestNoRememberingOptOut(t *testing.T) {
	if _, ok := reflect.TypeFor[Options]().FieldByName("DisableClientRemembering"); ok {
		t.Fatal("remembering still has an opt-out")
	}
}

func TestFailedInitializeDoesNotKeepSessionClient(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	wrap(t, s, Options{})
	tracked := lookup(s)
	ctx := context.Background()
	message := &struct {
		Header http.Header    `json:"-"`
		Params map[string]any `json:"params"`
	}{Header: http.Header{"Mcp-Session-Id": []string{"session"}}, Params: map[string]any{"clientInfo": &mcpbridge.Client{Name: "failed"}}}
	tracked.before(ctx, 1, "initialize", message)
	// A failed request never reaches succeeded; discard its pending call.
	tracked.take(ctx, message)
	if info, ok := tracked.sessions.load("session"); ok {
		t.Fatalf("failed initialize kept session state: %+v", info)
	}
}

func TestInitializeOrderKeepsNewestSessionClient(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	wrap(t, s, Options{})
	tracked := lookup(s)
	ctx := context.Background()
	begin := func(id int, name string) *call {
		message := &struct {
			Header http.Header    `json:"-"`
			Params map[string]any `json:"params"`
		}{Header: http.Header{"Mcp-Session-Id": []string{"session"}}, Params: map[string]any{"clientInfo": &mcpbridge.Client{Name: name}}}
		tracked.before(ctx, id, "initialize", message)
		return tracked.take(ctx, message)
	}
	older, newer := begin(1, "older"), begin(2, "newer")
	tracked.succeeded(ctx, newer, map[string]any{"protocolVersion": "2025-06-18"})
	tracked.succeeded(ctx, older, map[string]any{"protocolVersion": "2024-11-05"})
	info, ok := tracked.sessions.load("session")
	if !ok || info.client.Name != "newer" || info.version != "2025-06-18" {
		t.Fatalf("stale initialize overwrote session: %+v", info)
	}
}
