package rudderstackai

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestMCPRememberedClientPerRequestServer(t *testing.T) {
	for _, scenario := range []string{"same", "user", "oauth", "anonymous", "empty", "oauth_only", "user_only", "expired", "resolver", "meta", "latest", "failed", "server"} {
		t.Run(scenario, func(t *testing.T) {
			id := Identity{UserID: "user", OAuthClientID: "app"}
			if scenario == "anonymous" {
				id.UserID, id.AnonymousID = "", "anon"
			}
			if scenario == "empty" || scenario == "oauth_only" {
				id.UserID = ""
			}
			if scenario == "empty" || scenario == "user_only" {
				id.OAuthClientID = ""
			}
			opts := Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }}
			if scenario == "expired" {
				opts.ClientSeenWindow = time.Millisecond
			}
			initialize := func(name string) {
				s := workspaceServer(t.Name(), nil)
				wrap(t, s, opts)
				w, _ := instances.get(s)
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					if scenario == "failed" {
						return nil, context.Canceled
					}
					return &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}, nil
				})(context.Background(), "initialize", &mcp.ServerRequest[*mcp.InitializeParams]{
					Params: &mcp.InitializeParams{ClientInfo: &mcp.Implementation{Name: name, Version: "1"}},
					Extra:  &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: id.UserID}},
				})
				if err != nil && scenario != "failed" {
					t.Fatal(err)
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
				opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) { return "resolved", "1" }
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
			w, _ := instances.get(s)
			meta := mcp.Meta{}
			if scenario == "meta" {
				meta[metaClientInfo] = map[string]any{"name": "metadata", "version": "1"}
			}
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil })(
				context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "tool", Arguments: json.RawMessage(`{}`), Meta: meta}, Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: id.UserID}}})
			if err != nil {
				t.Fatal(err)
			}
			p := r.props(t, "mcp_tool_called")
			if str(p["client_name"]) != want || (want != "" && p["client_version"] != "1") {
				t.Errorf("client = %v, want %q", p, want)
			}
			if str(p["oauth_client_id"]) != id.OAuthClientID || p["auth_method"] != "oauth" {
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

func TestMCPRequestOAuthEveryEvent(t *testing.T) {
	for _, id := range []Identity{{OAuthClientID: "app", AuthMethod: "api_key", TokenScopes: []string{"private"}, Country: "IN"}, {}} {
		s := workspaceServer(t.Name(), nil)
		r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }, MCPEvents: true,
			CapabilityRequests: &CapabilityRequestOptions{Capabilities: []string{"export"}},
			EventRules:         EventRules{Tools: map[string]ToolEventRule{"tool": {Event: "performed"}}}})
		w, _ := instances.get(s)
		c := w.Begin(mcpbridge.Request{Ctx: context.Background()})
		c.ToolCall("tool", nil, false, nil, "")
		c.ToolDone(mcpbridge.Outcome{Result: &mcp.CallToolResult{}})
		c.Finished("resources/read", "resource", 0, "")
		c.Finished("prompts/get", "prompt", 0, "")
		c.Capability(nil)
		w.Elicited(c, mcpbridge.Elicitation{Action: "accept"})
		TrackMCPEventSubscription(context.Background(), s, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "sub", EventName: "update"})
		send := WrapMCPEventSender(s, func(context.Context, EventDelivery) (DeliveryOutcome, error) {
			return DeliveryOutcome{HTTPStatus: 200}, nil
		})
		if _, err := send(context.Background(), EventDelivery{SubscriptionID: "sub", EventID: "event", EventName: "update"}); err != nil {
			t.Fatal(err)
		}
		if len(r.all()) != 8 {
			t.Fatalf("events = %v", r.names())
		}
		for _, event := range r.all() {
			p := event.Properties
			if event.Event == "mcp_event_subscription_changed" || event.Event == "mcp_event_delivered" {
				for _, key := range []string{"oauth_client_id", "auth_method"} {
					if _, ok := p[key]; ok {
						t.Errorf("%s has %s", event.Event, key)
					}
				}
				continue
			}
			method := id.AuthMethod
			if method == "" {
				method = "none"
			}
			if str(p["oauth_client_id"]) != id.OAuthClientID || p["auth_method"] != method {
				t.Errorf("%s auth = %v", event.Event, p)
			}
			for _, key := range []string{"token_scopes", "country"} {
				if _, ok := p[key]; ok {
					t.Errorf("%s has %s", event.Event, key)
				}
			}
		}
	}
}

func TestMCPRememberedClientZeroWindowAndBound(t *testing.T) {
	freshState(t)
	r := newRecorder(t)
	var id Identity
	w, err := mcpbridge.Wrap(t.Name(), "1", Options{Analytics: r, Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }})
	if err != nil {
		t.Fatal(err)
	}
	core := w.(*wrapped)
	st := core.state()
	st.clients = newKeyedBoundedMap[rememberedClientKey, rememberedClient](2)
	initialize := func(user, name string) {
		id = Identity{UserID: user, OAuthClientID: "app"}
		c := w.Begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: name, Title: "private-title", Version: "1"}})
		c.Initialized(nil, []string{"roots"}, "2025-06-18")
	}
	initialize("first", "first-client")
	initialize("second", "second-client")
	initialize("third", "third-client")
	id.UserID = "first"
	if c := core.begin(mcpbridge.Request{Ctx: context.Background()}); c.base["client_name"] != nil || st.clients.size() != 2 {
		t.Errorf("oldest not evicted: %v, size %d", c.base, st.clients.size())
	}
	// The public zero window retains the default, including remembering.
	if core.opts.Load().ClientSeenWindow != 24*time.Hour {
		t.Fatal("zero window did not default to 24 hours")
	}
	id.UserID = "third"
	if c := core.begin(mcpbridge.Request{Ctx: context.Background()}); c.base["client_name"] != "third-client" {
		t.Fatalf("zero window default lost client: %v", c.base)
	}

}

func TestMCPRememberedClientKeyCollision(t *testing.T) {
	id := Identity{OAuthClientID: "app\x00user", UserID: "tail"}
	w, err := mcpbridge.Wrap(t.Name(), "1", Options{Analytics: newRecorder(t), Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w.Begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: "first"}}).Initialized(nil, nil, "")
	id = Identity{OAuthClientID: "app", UserID: "user\x00tail"}
	if c := w.(*wrapped).begin(mcpbridge.Request{Ctx: context.Background()}); c.base["client_name"] != nil {
		t.Fatalf("colliding identity inherited client: %v", c.base)
	}
}

func TestMCPRememberedClientInitializeOrder(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	type slowKey struct{}
	w, err := mcpbridge.Wrap(t.Name(), "1", Options{Analytics: newRecorder(t), Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		if ctx.Value(slowKey{}) != nil {
			close(entered)
			<-release
		}
		return Identity{OAuthClientID: "app", UserID: "user"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		w.Begin(mcpbridge.Request{Ctx: context.WithValue(context.Background(), slowKey{}, true), InitializeClient: &mcpbridge.Client{Name: "older"}}).Initialized(nil, nil, "")
	}()
	<-entered
	w.Begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: "newer"}}).Initialized(nil, nil, "")
	close(release)
	<-done
	if c := w.(*wrapped).begin(mcpbridge.Request{Ctx: context.Background()}); c.base["client_name"] != "newer" {
		t.Fatalf("stale initialize overwrote newer: %v", c.base)
	}
}

func TestMCPRememberedClientFixedExpiry(t *testing.T) {
	for _, scenario := range []string{"slow_identify", "rewrap_shorter", "rewrap_longer", "reads"} {
		t.Run(scenario, func(t *testing.T) {
			window := time.Hour
			if scenario == "slow_identify" || scenario == "rewrap_longer" {
				window = 20 * time.Millisecond
			}
			first := true
			identify := func(context.Context, *auth.TokenInfo) (Identity, error) {
				if scenario == "slow_identify" && first {
					time.Sleep(40 * time.Millisecond)
				}
				first = false
				return Identity{UserID: "user", OAuthClientID: "app"}, nil
			}
			opts := Options{Analytics: newRecorder(t), Identify: identify, ClientSeenWindow: window}
			w, err := mcpbridge.Wrap(t.Name(), "1", opts)
			if err != nil {
				t.Fatal(err)
			}
			c := w.(*wrapped).begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: "client"}})
			c.Initialized(nil, nil, "")
			entry, _ := c.st.clients.get(clientKey(c.identity))
			if scenario == "rewrap_shorter" {
				opts.ClientSeenWindow = time.Nanosecond
			}
			if scenario == "rewrap_longer" {
				opts.ClientSeenWindow = time.Hour
				time.Sleep(40 * time.Millisecond)
			}
			if err := w.Rewrap(opts); err != nil {
				t.Fatal(err)
			}
			got := w.(*wrapped).begin(mcpbridge.Request{Ctx: context.Background()})
			want := "client"
			if scenario == "slow_identify" || scenario == "rewrap_longer" {
				want = ""
			}
			if str(got.base["client_name"]) != want {
				t.Fatalf("client = %v, want %q", got.base, want)
			}
			if want != "" {
				after, _ := c.st.clients.get(clientKey(c.identity))
				if !entry.until.Equal(after.until) {
					t.Fatal("read or rewrap changed expiry")
				}
			}
		})
	}
}

func TestMCPNoRememberingOptOut(t *testing.T) {
	if _, ok := reflect.TypeFor[Options]().FieldByName("DisableClientRemembering"); ok {
		t.Fatal("remembering still has an opt-out")
	}
}

func TestMCPFailedInitializeDoesNotKeepSessionClient(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "initialize" && err == nil {
				return nil, context.Canceled
			}
			return res, err
		}
	})
	wrap(t, s, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	if _, err := mcp.NewClient(testClientInfo, nil).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"}); err == nil {
		t.Fatal("initialize unexpectedly succeeded")
	}
	w, _ := instances.get(s)
	h := w.before(context.Background(), "tools/call", &mcp.CallToolRequest{Session: ss, Params: &mcp.CallToolParamsRaw{Name: "tool"}})
	if h.rc.base["client_name"] != nil {
		t.Fatalf("failed initialize retained session client: %v", h.rc.base)
	}
}

func TestMCPInitializeOrderKeepsNewestSessionClient(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	wrap(t, s, Options{})
	w, _ := instances.get(s)
	ss := &mcp.ServerSession{}
	begin := func(name string) *handling {
		return w.before(context.Background(), "initialize", &mcp.ServerRequest[*mcp.InitializeParams]{Session: ss, Params: &mcp.InitializeParams{ClientInfo: &mcp.Implementation{Name: name}}})
	}
	older, newer := begin("older"), begin("newer")
	w.after(newer, &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}, nil, nil)
	w.after(older, &mcp.InitializeResult{ProtocolVersion: "2024-11-05"}, nil, nil)
	h := w.before(context.Background(), "tools/call", &mcp.CallToolRequest{Session: ss, Params: &mcp.CallToolParamsRaw{Name: "tool"}})
	if h.rc.base["client_name"] != "newer" || h.rc.base["protocol_version"] != "2025-06-18" {
		t.Fatalf("session did not keep newest initialize: %v", h.rc.base)
	}
}
