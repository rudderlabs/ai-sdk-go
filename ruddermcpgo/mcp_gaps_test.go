package ruddermcpgo

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Dispatch without a ClientSession: initialize and subsequent calls have no state key.
func TestStatelessClientAndIdentity(t *testing.T) {
	for _, source := range []string{"initialize", "meta", "empty_meta", "resolver", "empty_resolver", "panic", "unknown"} {
		t.Run(source, func(t *testing.T) {
			s := workspaceServer("stateless-"+source, nil)
			resolved := 0
			opts := Options{Identify: func(ctx context.Context) (rudderstackai.Identity, error) {
				call, ok := rudderstackai.ToolCallFromContext(ctx)
				if !ok {
					return rudderstackai.Identity{}, nil
				}
				if call.Name != "list_destinations" {
					t.Errorf("name = %s", call.Name)
				}
				var args map[string]string
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					t.Fatal(err)
				}
				return rudderstackai.Identity{AccountID: args["workspace_id"], UserID: "user_" + args["workspace_id"]}, nil
			}}
			if source != "unknown" {
				opts.ClientInfo = func(context.Context) (string, string) {
					resolved++
					if source == "panic" {
						panic("private error")
					}
					if source == "empty_resolver" {
						return "", ""
					}
					return "resolved-client", "2"
				}
			}
			r := wrap(t, s, opts)
			meta := map[string]any{}
			if source == "initialize" || source == "meta" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{"name": "meta-client", "version": "3"}
			}
			if source == "empty_meta" {
				meta["io.modelcontextprotocol/clientInfo"] = map[string]any{}
			}
			if source == "initialize" {
				response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "init-client", "version": "1"}, "_meta": meta}})))
				if _, ok := response.(mcp.JSONRPCError); ok {
					t.Fatalf("initialize = %v", response)
				}
				if p := r.props(t, "mcp_client_seen"); p["client_name"] != "init-client" || p["client_version"] != "1" {
					t.Errorf("initialize = %v", p)
				}
			}
			for i, workspace := range []string{"a", "b"} {
				response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": i + 2, "method": "tools/call", "params": map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": workspace}, "_meta": meta}})))
				if _, ok := response.(mcp.JSONRPCError); ok {
					t.Fatalf("tool = %v", response)
				}
			}
			calls := r.named("mcp_tool_called")
			if len(calls) != 2 {
				t.Fatalf("events = %v", r.names())
			}
			for i, workspace := range []string{"a", "b"} {
				p := calls[i].Properties
				if p["account_id"] != workspace || calls[i].UserId != "user_"+workspace {
					t.Errorf("identity = %v", calls[i])
				}
				if _, ok := p["mcp_session_id"]; ok {
					t.Errorf("session = %v", p)
				}
				if _, ok := p["retry_count"]; ok {
					t.Errorf("retries = %v", p)
				}
				want := ""
				if source == "meta" || source == "initialize" {
					want = "meta-client"
				}
				if source == "resolver" || source == "empty_meta" {
					want = "resolved-client"
				}
				if want == "" {
					if _, ok := p["client_name"]; ok {
						t.Errorf("client = %v", p)
					}
				} else if p["client_name"] != want {
					t.Errorf("client = %v", p)
				}
			}
			s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/read", "params": map[string]any{"uri": "docs://readme", "_meta": meta}})))
			event := r.named("mcp_resource_read")[0]
			if _, ok := event.Properties["account_id"]; ok || event.UserId != "" {
				t.Errorf("identity leaked: %v", event)
			}
			wantResolved := 0
			if source == "resolver" || source == "empty_meta" || source == "empty_resolver" || source == "panic" {
				wantResolved = 3
			}
			if resolved != wantResolved {
				t.Errorf("resolver calls = %d, want %d", resolved, wantResolved)
			}
		})
	}
}

func TestClientResolverSessionPrecedence(t *testing.T) {
	s := workspaceServer("resolver-session", nil)
	r := wrap(t, s, Options{ClientInfo: func(context.Context) (string, string) {
		t.Error("resolver called with session client")
		return "wrong", "wrong"
	}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if p := r.props(t, "mcp_tool_called"); p["client_name"] != "test-client" {
		t.Errorf("client = %v", p)
	}
}

func TestToolErrorStatusParity(t *testing.T) {
	for _, hint := range []string{"meta", "status", "statusCode"} {
		for _, tc := range []struct {
			status any
			want   string
		}{{401, "auth_error"}, {403, "denied"}, {408, "timeout"}, {504, "timeout"}, {429, "rate_limit"}, {404, "invalid_request"}, {500, "api_error"}, {nil, "api_error"}, {"429", "api_error"}} {
			t.Run(fmt.Sprintf("%s/%v", hint, tc.status), func(t *testing.T) {
				s := workspaceServer("status-test", nil)
				result := mcp.NewToolResultError("private error text")
				if hint == "meta" {
					result.Meta = &mcp.Meta{AdditionalFields: map[string]any{"com.rudderstack/http_status": tc.status}}
				} else {
					result.StructuredContent = map[string]any{hint: tc.status}
				}
				s.AddTool(mcp.NewTool("tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil })
				r := wrap(t, s, Options{})
				s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool","arguments":{}}}`))
				if p := r.props(t, "mcp_tool_called"); p["error_type"] != tc.want {
					t.Errorf("error = %v", p)
				}
				if strings.Contains(mustJSON(r.all()), "private error text") {
					t.Error("error text leaked")
				}
			})
		}
	}
}

func TestToolErrorClassifier(t *testing.T) {
	for _, kind := range []string{"rate_limit", "timeout", "cancelled", "denied", "auth_error", "invalid_request", "connection_error", "api_error", "unsupported", "", "panic"} {
		t.Run(kind, func(t *testing.T) {
			s := workspaceServer("classifier-test", nil)
			result := mcp.NewToolResultError("private error text")
			result.StructuredContent = map[string]any{"status": 401}
			s.AddTool(mcp.NewTool("tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil })
			count := 0
			r := wrap(t, s, Options{ClassifyToolError: func(name string, got *mcp.CallToolResult) string {
				count++
				if name != "tool" || got != result {
					t.Error("classifier arguments")
				}
				if kind == "panic" {
					panic("private error")
				}
				return kind
			}})
			invoke := func() {
				s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool","arguments":{}}}`))
			}
			invoke()
			want := kind
			if kind == "unsupported" || kind == "" || kind == "panic" {
				want = "auth_error"
			}
			if p := r.props(t, "mcp_tool_called"); p["error_type"] != want {
				t.Errorf("classification = %v", p)
			}
			result.IsError = false
			invoke()
			if count != 1 {
				t.Errorf("classifier calls = %d", count)
			}
		})
	}
}

func TestStatelessClientEveryRequestType(t *testing.T) {
	for _, source := range []string{"meta", "resolver"} {
		for _, method := range []string{"ping", "tools/list", "resources/list", "resources/templates/list", "prompts/list", "resources/read", "prompts/get"} {
			t.Run(source+"/"+method, func(t *testing.T) {
				s := workspaceServer("client-types", nil)
				resolved := 0
				r := wrap(t, s, Options{ClientInfo: func(context.Context) (string, string) { resolved++; return "client", "1" }})
				params := map[string]any{"uri": "docs://readme", "name": "audit_tracking_plan"}
				if source == "meta" {
					params["_meta"] = map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "client", "version": "1"}}
				}
				response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})))
				if _, ok := response.(mcp.JSONRPCError); ok {
					t.Fatalf("response = %v", response)
				}
				for _, event := range r.all() {
					if event.Properties["client_name"] != "client" || event.Properties["client_version"] != "1" {
						t.Errorf("client = %v", event.Properties)
					}
				}
				want := 0
				if source == "resolver" {
					want = 1
				}
				if resolved != want {
					t.Errorf("resolver calls = %d, want %d", resolved, want)
				}
			})
		}
	}
}

func TestClientInfoFallback(t *testing.T) {
	for _, session := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			info        map[string]any
			wantName    string
			wantVersion string
		}{
			{"title_only", map[string]any{"title": "Client Title"}, "", ""},
			{"whitespace_only", map[string]any{"name": " \t\n", "version": "\u2003 ", "title": "Client Title"}, "", ""},
			{"name_only", map[string]any{"name": " client ", "version": " \t"}, "client", ""},
			{"version_only", map[string]any{"name": " \t", "version": " 3 "}, "", "3"},
		} {
			t.Run(fmt.Sprintf("%s/session=%v", tc.name, session), func(t *testing.T) {
				s := workspaceServer("client-fallback", nil)
				resolved := 0
				r := wrap(t, s, Options{ClientInfo: func(context.Context) (string, string) {
					resolved++
					return "resolved-client", "2"
				}})
				meta := sdk.Meta{"io.modelcontextprotocol/clientInfo": tc.info}

				if session {
					cs := connectStdio(t, s, "2025-06-18", nil)
					callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
				} else {
					response := s.HandleMessage(context.Background(), []byte(mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_destinations", "arguments": map[string]any{"workspace_id": "w"}, "_meta": meta}})))
					if _, ok := response.(mcp.JSONRPCError); ok {
						t.Fatalf("tool = %v", response)
					}
				}

				wantName, wantVersion := tc.wantName, tc.wantVersion
				wantResolved := 0
				if wantName == "" && wantVersion == "" {
					if session {
						wantName, wantVersion = "test-client", "1.2.3"
					} else {
						wantName, wantVersion = "resolved-client", "2"
						wantResolved = 1
					}
				}
				p := r.props(t, "mcp_tool_called")
				if strName, _ := p["client_name"].(string); strName != wantName {
					t.Errorf("client_name = %v, want %q", p["client_name"], wantName)
				}
				if strVersion, _ := p["client_version"].(string); strVersion != wantVersion {
					t.Errorf("client_version = %v, want %q", p["client_version"], wantVersion)
				}
				if resolved != wantResolved {
					t.Errorf("resolver calls = %d, want %d", resolved, wantResolved)
				}
			})
		}
	}
}

func TestInvalidArgumentsWithErrorClassification(t *testing.T) {
	for _, valid := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			classified string
			status     any
			isError    bool
			want       string
		}{
			{"classifier", "denied", 429, true, "denied"},
			{"status", "", 429, true, "rate_limit"},
			{"fallback", "", nil, true, ""},
			{"success", "denied", 429, false, ""},
		} {
			t.Run(fmt.Sprintf("%s/valid=%v", tc.name, valid), func(t *testing.T) {
				s := workspaceServer("invalid-classification", nil)

				result := mcp.NewToolResultText("ok")
				result.IsError = tc.isError
				result.StructuredContent = map[string]any{"status": tc.status}
				s.AddTool(mcp.NewTool("tool", mcp.WithString("workspace_id", mcp.Required())),
					func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil })
				r := wrap(t, s, Options{ClassifyToolError: func(string, *mcp.CallToolResult) string { return tc.classified }})
				cs := connectStdio(t, s, "2025-06-18", nil)

				if _, err := cs.ListTools(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"workspace_id": any(map[string]any{"secret": "private-value"})}
				if valid {
					args["workspace_id"] = "private-value"
				}
				callTool(t, cs, "tool", args, nil)
				p := r.props(t, "mcp_tool_called")
				want := tc.want
				if tc.isError && want == "" {
					want = "api_error"
					if !valid {
						want = "invalid_request"
					}
				}
				if got, _ := p["error_type"].(string); got != want {
					t.Errorf("error_type = %v, want %q", p["error_type"], want)
				}
				if tc.isError && !valid {
					got, _ := p["invalid_arguments"].([]string)
					if !slices.Equal(got, []string{"workspace_id"}) {
						t.Errorf("invalid_arguments = %v", p["invalid_arguments"])
					}
				} else if _, exists := p["invalid_arguments"]; exists {
					t.Errorf("unexpected invalid_arguments = %v", p["invalid_arguments"])
				}
				if strings.Contains(mustJSON(r.all()), "private-value") {
					t.Error("argument value leaked")
				}
			})
		}
	}
}
