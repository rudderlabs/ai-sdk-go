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

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type userAgentTransport = testkit.UserAgentTransport

func TestMCPUserAgentLegacySSEFallback(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{Transport: "sse"})
	ts := httptest.NewServer(server.NewSSEServer(s))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := sdk.NewClient(&sdk.Implementation{}, nil).Connect(ctx,
		&sdk.SSEClientTransport{Endpoint: ts.URL + "/sse", HTTPClient: &http.Client{Transport: userAgentTransport{}}},
		&sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_destinations", Arguments: map[string]any{"workspace_id": "w"}})
	if err != nil || res.IsError {
		t.Fatalf("SSE tool call: result=%v error=%v", res, err)
	}
	for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
		p := r.props(t, event)
		if p["client_name"] != "Cursor" || p["client_version"] != "1.7.4" {
			t.Errorf("%s did not resolve SSE User-Agent: %v", event, p)
		}
	}
}

func TestMCPUserAgentPreservesTitleOnlySessionClient(t *testing.T) {
	for _, ua := range []string{"", " \t", "Cursor/1.7.4"} {
		t.Run(ua, func(t *testing.T) {
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, Options{Identify: func(context.Context) (rudderstackai.Identity, error) {
				return rudderstackai.Identity{UserID: "user"}, nil
			}})
			lookup(s).sessions.store(`["","user","","session"]`, sessionInfo{client: &mcpbridge.Client{Title: "Session Client Title"}, version: "2025-06-18"})
			body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
				"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"},
				"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2025-06-18"},
			}})
			req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Session-Id", "session")
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
			req.Header.Set("Mcp-Method", "tools/call")
			req.Header.Set("Mcp-Name", "list_destinations")
			if ua != "" {
				req.Header.Set("User-Agent", ua)
			}
			response := httptest.NewRecorder()
			server.NewStreamableHTTPServer(s, server.WithStateLess(true)).ServeHTTP(response, req)
			if response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
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

func TestMCPUserAgentFallback(t *testing.T) {
	for _, scenario := range []string{"known", "seen", "unknown", "app", "app_version_only", "empty", "blank", "panic", "absent", "metadata", "resolver", "title_only", "title_blank_labels", "title_unknown", "title_app", "title_empty"} {
		t.Run(scenario, func(t *testing.T) {
			ua, wantName, wantVersion := "claude-code/2.1.322 (cli)", "claude-code", "2.1.322"
			calls := 0
			opts := Options{}
			if scenario == "app" || scenario == "app_version_only" || scenario == "empty" || scenario == "blank" || scenario == "panic" || scenario == "title_app" || scenario == "title_empty" {
				opts.ClientInfoFallback = func(context.Context) (string, string) {
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
				opts.ClientInfo = func(context.Context) (string, string) { return "resolver", "5" }
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
			body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}, "_meta": meta}})
			if ua == "" {
				s.HandleMessage(context.Background(), []byte(body))
			} else {
				req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("User-Agent", ua)
				if strings.HasPrefix(scenario, "title_") {
					req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
					req.Header.Set("Mcp-Method", "tools/call")
					req.Header.Set("Mcp-Name", "list_destinations")
				}
				response := httptest.NewRecorder()
				server.NewStreamableHTTPServer(s, server.WithStateLess(true)).ServeHTTP(response, req)
				if response.Code != 200 {
					t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
				}
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
				if stringProperty(p, "client_name") != wantName || stringProperty(p, "client_version") != wantVersion {
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
			opts := Options{Identify: func(context.Context) (rudderstackai.Identity, error) {
				return rudderstackai.Identity{UserID: "user", OAuthClientID: "app"}, nil
			}}
			request := func(method string, params map[string]any) *recorder {
				t.Helper()
				s := workspaceServer(t.Name(), nil)
				r := wrap(t, s, opts)
				body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
				req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("User-Agent", "Cursor/1.7.4")
				response := httptest.NewRecorder()
				server.NewStreamableHTTPServer(s, server.WithStateLess(true)).ServeHTTP(response, req)
				if response.Code != 200 || strings.Contains(response.Body.String(), `"error"`) {
					t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
				}
				return r
			}
			client := map[string]any{}
			wantName, wantVersion := "Cursor", "1.7.4"
			if scenario == "blank_after_remembered" {
				request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "old-client", "version": "2"}})
			}
			if scenario == "title_only" {
				client["title"] = "Only A Title"
			}
			if scenario == "known_initialize" || scenario == "remembered" {
				client = map[string]any{"name": "initialize-client", "version": "4"}
				wantName, wantVersion = "initialize-client", "4"
			}
			r := request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": client})
			p := r.props(t, "mcp_client_seen")
			if stringProperty(p, "client_name") != wantName || stringProperty(p, "client_version") != wantVersion {
				t.Errorf("initialize attribution = %v, want %q/%q", p, wantName, wantVersion)
			}
			if scenario == "title_only" && p["client_title"] != "Only A Title" {
				t.Errorf("initialize title lost: %v", p)
			}
			if scenario != "remembered" {
				return
			}
			r = request("tools/call", map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}})
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
