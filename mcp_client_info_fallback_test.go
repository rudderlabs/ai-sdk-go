package rudderstackai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPClientInfoFallbackResolver(t *testing.T) {
	for _, scenario := range []string{"alone", "ordinary", "remembered", "resolver", "empty_resolver", "panic_resolver", "meta", "panic", "blank"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			token := &auth.TokenInfo{UserID: "user"}
			opts := Options{
				Identify: func(context.Context, *auth.TokenInfo) (Identity, error) {
					return Identity{UserID: "user", OAuthClientID: "app"}, nil
				},
				ClientInfoFallback: func(ctx context.Context, tok *auth.TokenInfo) (string, string) {
					calls++
					if tok != token {
						t.Error("fallback did not receive request token")
					}
					if call, ok := ToolCallFromContext(ctx); !ok || call.Name != "tool" {
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
				w, _ := instances.get(s)
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					return &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}, nil
				})(context.Background(), "initialize", &mcp.ServerRequest[*mcp.InitializeParams]{
					Params: &mcp.InitializeParams{ClientInfo: &mcp.Implementation{Name: "remembered", Version: "1"}},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "resolver" || scenario == "empty_resolver" || scenario == "panic_resolver" {
				opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) {
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
			w, _ := instances.get(s)
			meta := mcp.Meta{}
			if scenario != "remembered" && scenario != "resolver" && scenario != "ordinary" {
				meta[metaClientInfo] = map[string]any{}
			}
			if scenario == "meta" {
				meta[metaClientInfo] = map[string]any{"name": "metadata", "version": "4"}
			}
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
				return &mcp.CallToolResult{}, nil
			})(context.Background(), "tools/call", &mcp.CallToolRequest{
				Params: &mcp.CallToolParamsRaw{Name: "tool", Arguments: json.RawMessage(`{}`), Meta: meta},
				Extra:  &mcp.RequestExtra{TokenInfo: token},
			})
			if err != nil {
				t.Fatal(err)
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
				if str(p["client_name"]) != wantName || str(p["client_version"]) != wantVersion {
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

func TestMCPClientInfoFallbackSessionPrecedence(t *testing.T) {
	s := workspaceServer(t.Name(), nil)
	r := wrap(t, s, Options{ClientInfoFallback: func(context.Context, *auth.TokenInfo) (string, string) {
		t.Error("fallback called with initialize or session client")
		return "wrong", "wrong"
	}})
	cs := connect(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if p := r.props(t, "mcp_tool_called"); p["client_name"] != "test-client" {
		t.Errorf("client = %v", p)
	}
}
