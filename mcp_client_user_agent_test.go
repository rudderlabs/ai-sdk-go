package rudderstackai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type userAgentTransport = testkit.UserAgentTransport

func TestMCPUserAgentLegacySSEHeaderLimit(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{Transport: "sse"})
	ts := httptest.NewServer(mcp.NewSSEHandler(func(req *http.Request) *mcp.Server {
		if req.UserAgent() != "Cursor/1.7.4" {
			t.Errorf("HTTP User-Agent = %q", req.UserAgent())
		}
		return s
	}, nil))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{}, nil).Connect(ctx,
		&mcp.SSEClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: userAgentTransport{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_destinations", Arguments: map[string]any{"workspace_id": "w"}})
	if err != nil || res.IsError {
		t.Fatalf("SSE tool call: result=%v error=%v", res, err)
	}
	for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
		p := r.props(t, event)
		if p["client_name"] != nil || p["client_version"] != nil {
			t.Errorf("%s unexpectedly resolved SSE User-Agent: %v", event, p)
		}
	}
}

func TestMCPUserAgentPreservesTitleOnlySessionClient(t *testing.T) {
	for _, ua := range []string{"", " \t", "Cursor/1.7.4"} {
		t.Run(ua, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) {
				return Identity{UserID: "user"}, nil
			}})
			ss := &mcp.ServerSession{}
			rememberInitialized(ss, &mcpbridge.Client{Title: "Session Client Title"}, "2025-06-18", 1, callerStateKey("", "user", ""))
			w, _ := instances.get(s)
			header := http.Header{"Content-Type": []string{"application/json"}}
			if ua != "" {
				header.Set("User-Agent", ua)
			}
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
				return &mcp.CallToolResult{}, nil
			})(context.Background(), "tools/call", &mcp.CallToolRequest{
				Session: ss,
				Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(`{"workspace_id":"w"}`),
					Meta: mcp.Meta{metaProtocolVersion: "2026-07-28"}},
				Extra: &mcp.RequestExtra{Header: header},
			})
			if err != nil {
				t.Fatal(err)
			}
			p := r.props(t, "mcp_client_seen")
			if ua == "Cursor/1.7.4" {
				if p["client_name"] != "Cursor" || p["client_version"] != "1.7.4" || p["client_title"] != nil {
					t.Errorf("User-Agent client = %v", p)
				}
			} else if p["client_title"] != "Session Client Title" || p["client_name"] != nil || p["client_version"] != nil {
				t.Errorf("title-only session client lost: %v", p)
			}
		})
	}
}

func TestClientFromUserAgent(t *testing.T) {
	var fixture struct {
		Products       map[string]string `json:"products"`
		VersionPattern string            `json:"version_pattern"`
		Versionless    []string          `json:"versionless_products"`
		Cases          []struct {
			Name      string            `json:"name"`
			UserAgent *string           `json:"user_agent"`
			Expected  map[string]string `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-client-user-agent-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(userAgentClients, fixture.Products) {
		t.Fatal("product table differs from shared fixture")
	}
	if versionPattern.String() != fixture.VersionPattern {
		t.Fatal("version pattern differs from shared fixture")
	}
	versionless := map[string]bool{}
	for _, product := range fixture.Versionless {
		versionless[product] = true
	}
	if !reflect.DeepEqual(userAgentVersionless, versionless) {
		t.Fatal("versionless products differ from shared fixture")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			ua := ""
			if tc.UserAgent != nil {
				ua = *tc.UserAgent
			}
			client := clientFromUserAgent(ua)
			var got map[string]string
			if client != nil {
				got = map[string]string{"client_name": client.Name}
				if client.Version != "" {
					got["client_version"] = client.Version
				}
			}
			if !reflect.DeepEqual(got, tc.Expected) {
				t.Errorf("client = %v, want %v", got, tc.Expected)
			}
		})
	}
}

func TestMCPUserAgentFallback(t *testing.T) {
	for _, scenario := range []string{"known", "seen", "unknown", "app", "app_version_only", "empty", "blank", "panic", "absent", "metadata", "resolver", "title_only", "title_blank_labels", "title_unknown", "title_app", "title_empty"} {
		t.Run(scenario, func(t *testing.T) {
			ua, wantName, wantVersion := "claude-code/2.1.322 (cli)", "claude-code", "2.1.322"
			calls := 0
			opts := Options{}
			if scenario == "app" || scenario == "app_version_only" || scenario == "empty" || scenario == "blank" || scenario == "panic" || scenario == "title_app" || scenario == "title_empty" {
				opts.ClientInfoFallback = func(context.Context, *auth.TokenInfo) (string, string) {
					calls++
					switch scenario {
					case "app", "title_app":
						return "app-client", "3"
					case "app_version_only":
						return "", "3"
					case "blank":
						return " \t", "\n"
					case "panic":
						panic("boom")
					default:
						return "", ""
					}
				}
			}
			switch scenario {
			case "app", "title_app":
				wantName, wantVersion = "app-client", "3"
			case "app_version_only":
				wantName, wantVersion = "", "3"
			case "unknown", "title_unknown":
				ua, wantName, wantVersion = "jane.doe@example.com/9.9 secret-agent", "other", ""
			case "absent":
				ua, wantName, wantVersion = "", "", ""
			case "metadata":
				wantName, wantVersion = "metadata", "4"
			case "resolver":
				opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) { return "resolver", "5" }
				wantName, wantVersion = "resolver", "5"
			}
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, opts)
			meta := map[string]any{}
			if scenario == "seen" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{}
			}
			if scenario == "metadata" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "metadata", "version": "4"}
			}
			if strings.HasPrefix(scenario, "title_") {
				info := map[string]any{"title": "Request Client Title"}
				if scenario == "title_blank_labels" {
					info["name"], info["version"] = " \t", "\n"
				}
				meta["io.modelcontextprotocol/clientInfo"] = info
				meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"
				meta["io.modelcontextprotocol/clientCapabilities"] = map[string]any{}
			}
			w, _ := instances.get(s)
			var header http.Header
			if ua != "" {
				header = http.Header{"User-Agent": []string{ua}}
				if strings.HasPrefix(scenario, "title_") {
					header.Set("Mcp-Protocol-Version", "2026-07-28")
					header.Set("Mcp-Method", "tools/call")
					header.Set("Mcp-Name", "list_destinations")
				}
			}
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{
				Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(`{"workspace_id":"w"}`), Meta: mcp.Meta(meta)},
				Extra:  &mcp.RequestExtra{Header: header},
			})
			if err != nil {
				t.Fatal(err)
			}
			if opts.ClientInfoFallback != nil && calls != 1 {
				t.Errorf("fallback calls = %d, want 1", calls)
			}
			events := []string{"mcp_tool_called"}
			if scenario == "seen" || scenario == "metadata" || strings.HasPrefix(scenario, "title_") {
				events = append(events, "mcp_client_seen")
			} else if len(r.named("mcp_client_seen")) != 0 {
				t.Error("ordinary request emitted client-seen")
			}
			for _, event := range events {
				p := r.props(t, event)
				if str(p["client_name"]) != wantName || str(p["client_version"]) != wantVersion {
					t.Errorf("%s client = %v, want %q/%q", event, p, wantName, wantVersion)
				}
				if wantName == "" {
					if _, ok := p["client_name"]; ok {
						t.Errorf("%s has unexpected client_name", event)
					}
				}
				if wantVersion == "" {
					if _, ok := p["client_version"]; ok {
						t.Errorf("%s has unexpected client_version", event)
					}
				}
				if event == "mcp_client_seen" && strings.HasPrefix(scenario, "title_") && p["client_title"] != "Request Client Title" {
					t.Errorf("request title lost: %v", p)
				}
				if _, ok := p["mcp_session_id"]; ok {
					t.Errorf("%s has stateless session ID", event)
				}
			}
			if scenario == "unknown" && strings.Contains(mustJSON(r.all()), "jane.doe") {
				t.Error("raw User-Agent leaked into events")
			}
		})
	}
}

func TestMCPUserAgentInitializeAndRememberedClient(t *testing.T) {
	for _, scenario := range []string{"blank_initialize", "blank_after_remembered", "title_only", "known_initialize", "remembered"} {
		t.Run(scenario, func(t *testing.T) {
			opts := Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) {
				return Identity{UserID: "user", OAuthClientID: "app"}, nil
			}}
			initialize := func(client *mcp.Implementation) *recorder {
				t.Helper()
				s := workspaceServer(t.Name(), nil)
				r := wrap(t, s, opts)
				w, _ := instances.get(s)
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					return &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}, nil
				})(context.Background(), "initialize", &mcp.ServerRequest[*mcp.InitializeParams]{
					Params: &mcp.InitializeParams{ClientInfo: client},
					Extra:  &mcp.RequestExtra{Header: http.Header{"User-Agent": []string{"Cursor/1.7.4"}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			client := &mcp.Implementation{}
			wantName, wantVersion := "Cursor", "1.7.4"
			if scenario == "blank_after_remembered" {
				initialize(&mcp.Implementation{Name: "old-client", Version: "2"})
			}
			if scenario == "title_only" {
				client.Title = "Only A Title"
			}
			if scenario == "known_initialize" || scenario == "remembered" {
				client = &mcp.Implementation{Name: "initialize-client", Version: "4"}
				wantName, wantVersion = client.Name, client.Version
			}
			r := initialize(client)
			p := r.props(t, "mcp_client_seen")
			if str(p["client_name"]) != wantName || str(p["client_version"]) != wantVersion {
				t.Errorf("initialize attribution = %v, want %q/%q", p, wantName, wantVersion)
			}
			if scenario == "title_only" && p["client_title"] != "Only A Title" {
				t.Errorf("initialize title lost: %v", p)
			}
			if scenario != "remembered" {
				return
			}
			s := workspaceServer(t.Name(), nil)
			r = wrap(t, s, opts)
			w, _ := instances.get(s)
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{
				Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(`{"workspace_id":"w"}`)},
				Extra:  &mcp.RequestExtra{Header: http.Header{"User-Agent": []string{"Cursor/1.7.4"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			p = r.props(t, "mcp_tool_called")
			if p["client_name"] != "initialize-client" || p["client_version"] != "4" {
				t.Errorf("remembered client lost precedence: %v", p)
			}
			if len(r.named("mcp_client_seen")) != 0 {
				t.Error("remembered client emitted client-seen")
			}
		})
	}
}

func TestClientFromUserAgentASCIIAndBoundaries(t *testing.T) {
	for _, tc := range []struct{ ua, name, version string }{
		{"\u00a0claude-code/1.0", "other", ""},
		{"\nclaude-code/1.0", "other", ""},
		{"curſor/1.0", "other", ""},
		{" \t ", "", ""},
		{"opencode/" + strings.Repeat("1", 32), "opencode", strings.Repeat("1", 32)},
		{"opencode/" + strings.Repeat("1", 33), "opencode", ""},
		{"cursor/1.0\u00a0", "Cursor", ""},
	} {
		t.Run(tc.ua, func(t *testing.T) {
			client := clientFromUserAgent(tc.ua)
			if tc.name == "" {
				if client != nil {
					t.Errorf("client = %v, want nil", client)
				}
				return
			}
			if client == nil || client.Name != tc.name || client.Version != tc.version {
				t.Errorf("client = %v, want %q/%q", client, tc.name, tc.version)
			}
		})
	}
}
