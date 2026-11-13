package ruddermcpgo

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestClientInfoFallbackResolver(t *testing.T) {
	for _, scenario := range []string{"alone", "ordinary", "remembered", "resolver", "empty_resolver", "panic_resolver", "meta", "panic", "blank"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			opts := Options{
				Identify: func(context.Context) (rudderstackai.Identity, error) {
					return rudderstackai.Identity{UserID: "user", OAuthClientID: "app"}, nil
				},
				ClientInfoFallback: func(ctx context.Context) (string, string) {
					calls++
					if call, ok := rudderstackai.ToolCallFromContext(ctx); !ok || call.Name != "list_destinations" {
						t.Error("fallback did not receive tool context")
					}
					if scenario == "panic" {
						panic("fallback failed")
					}
					if scenario == "blank" {
						return " \t", "\n"
					}
					return "fallback", "2"
				},
			}
			if scenario == "remembered" || scenario == "resolver" {
				s := workspaceServer(t.Name(), nil)
				wrap(t, s, opts)
				response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{
					"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
						"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
						"clientInfo": map[string]any{"name": "remembered", "version": "1"},
					},
				})))
				if _, failed := response.(mcp.JSONRPCError); failed {
					t.Fatal(response)
				}
			}
			if scenario == "resolver" || scenario == "empty_resolver" || scenario == "panic_resolver" {
				opts.ClientInfo = func(context.Context) (string, string) {
					if scenario == "panic_resolver" {
						panic("resolver failed")
					}
					if scenario == "empty_resolver" {
						return "", ""
					}
					return "resolver", "3"
				}
			}
			s := workspaceServer(t.Name(), nil)
			r := wrap(t, s, opts)
			meta := map[string]any{}
			if scenario != "remembered" && scenario != "resolver" && scenario != "ordinary" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{}
			}
			if scenario == "meta" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "metadata", "version": "4"}
			}
			response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{
				"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
					"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}, "_meta": meta,
				},
			})))
			if _, failed := response.(mcp.JSONRPCError); failed {
				t.Fatal(response)
			}
			wantName, wantVersion, wantCalls := "fallback", "2", 1
			switch scenario {
			case "remembered":
				wantName, wantVersion, wantCalls = "remembered", "1", 0
			case "resolver":
				wantName, wantVersion, wantCalls = "resolver", "3", 0
			case "meta":
				wantName, wantVersion, wantCalls = "metadata", "4", 0
			case "panic", "blank":
				wantName, wantVersion = "", ""
			}
			if calls != wantCalls {
				t.Errorf("fallback calls = %d, want %d", calls, wantCalls)
			}
			assertClient := func(event string) {
				p := r.props(t, event)
				if stringProperty(p, "client_name") != wantName || stringProperty(p, "client_version") != wantVersion {
					t.Errorf("%s client = %v, want %q/%q", event, p, wantName, wantVersion)
				}
				if wantName == "" {
					for _, key := range []string{"client_name", "client_version"} {
						if _, ok := p[key]; ok {
							t.Errorf("%s has unexpected %s", event, key)
						}
					}
				}
			}
			assertClient("mcp_tool_called")
			if scenario == "remembered" || scenario == "resolver" || scenario == "ordinary" {
				if len(r.named("mcp_client_seen")) != 0 {
					t.Error("ordinary request unexpectedly emitted client-seen")
				}
			} else {
				assertClient("mcp_client_seen")
			}
		})
	}
}

func TestClientInfoFallbackSessionPrecedence(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{ClientInfoFallback: func(context.Context) (string, string) {
		t.Error("fallback called with initialize or session client")
		return "wrong", "wrong"
	}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if p := r.props(t, "mcp_tool_called"); p["client_name"] != "test-client" {
		t.Errorf("client = %v", p)
	}
}
