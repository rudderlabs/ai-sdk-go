package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestMCPStatelessClientAndIdentity(t *testing.T) {
	for _, source := range []string{"initialize", "meta", "empty_meta", "resolver", "empty_resolver", "panic", "unknown"} {
		t.Run(source, func(t *testing.T) {
			s := workspaceServer("stateless-"+source, nil)
			resolved := 0
			opts := Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
				call, ok := ToolCallFromContext(ctx)
				if !ok {
					return Identity{}, nil
				}
				if call.Name != "list_destinations" {
					t.Errorf("name = %s", call.Name)
				}
				var args map[string]string
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					t.Fatal(err)
				}
				return Identity{AccountID: args["workspace_id"], UserID: "user_" + args["workspace_id"]}, nil
			}}
			if source != "unknown" {
				opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) {
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
			w, _ := instances.get(s)
			meta := mcp.Meta{}
			if source == "meta" || source == "initialize" {
				meta[metaClientInfo] = map[string]any{"name": "meta-client", "version": "3"}
			}
			if source == "empty_meta" {
				meta[metaClientInfo] = map[string]any{}
			}
			if source == "initialize" {
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					return &mcp.InitializeResult{ProtocolVersion: "2025-06-18"}, nil
				})(context.Background(), "initialize", &mcp.InitializeRequest{Params: &mcp.InitializeParams{Meta: meta, ClientInfo: &mcp.Implementation{Name: "init-client", Version: "1"}}})
				if err != nil {
					t.Fatal(err)
				}
				if p := r.props(t, "mcp_client_seen"); p["client_name"] != "init-client" || p["client_version"] != "1" {
					t.Errorf("initialize = %v", p)
				}
			}
			for _, workspace := range []string{"a", "b"} {
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
				})(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(fmt.Sprintf(`{"workspace_id":%q}`, workspace)), Meta: meta}})
				if err != nil {
					t.Fatal(err)
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
					t.Errorf("stateless session = %v", p)
				}
				if _, ok := p["retry_count"]; ok {
					t.Errorf("stateless retries = %v", p)
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
			_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.ReadResourceResult{}, nil })(context.Background(), "resources/read", &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "docs://readme", Meta: meta}})
			if err != nil {
				t.Fatal(err)
			}
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

func TestMCPClientResolverSessionPrecedence(t *testing.T) {
	s := workspaceServer("resolver-session", nil)
	r := wrap(t, s, Options{ClientInfo: func(context.Context, *auth.TokenInfo) (string, string) {
		t.Error("resolver called with session client")
		return "wrong", "wrong"
	}})
	cs := connect(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if p := r.props(t, "mcp_tool_called"); p["client_name"] != "test-client" {
		t.Errorf("client = %v", p)
	}
}

func TestMCPServerVersionEveryEvent(t *testing.T) {
	for _, version := range []string{"1.2.3", ""} {
		t.Run(version, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: "version-test", Version: version}, nil)
			r := wrap(t, s, Options{MCPEvents: true, CapabilityRequests: &CapabilityRequestOptions{Capabilities: []string{"export"}}})
			server, _ := instances.get(s)
			c := server.Begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: "client"}})
			c.Initialized(nil, nil, "2025-06-18")
			c.ToolCall("tool", nil, false, nil, "")
			c.ToolDone(mcpbridge.Outcome{Result: &mcp.CallToolResult{}})
			c.Finished("resources/read", "resource", 0, "")
			c.Finished("prompts/get", "prompt", 0, "")
			c.Capability(nil)
			server.Elicited(c, mcpbridge.Elicitation{Action: "accept"})
			server.Elicited(nil, mcpbridge.Elicitation{Action: "accept"})
			TrackMCPEventSubscription(context.Background(), s, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "sub", EventName: "update"})
			send := WrapMCPEventSender(s, func(context.Context, EventDelivery) (DeliveryOutcome, error) {
				return DeliveryOutcome{HTTPStatus: 200}, nil
			})
			if _, err := send(context.Background(), EventDelivery{SubscriptionID: "sub", EventID: "event", EventName: "update"}); err != nil {
				t.Fatal(err)
			}
			if len(r.all()) != 9 {
				t.Fatalf("events = %v", r.names())
			}
			for _, event := range r.all() {
				got, exists := event.Properties["server_version"]
				if version == "" && exists || version != "" && got != version {
					t.Errorf("%s version = %v", event.Event, got)
				}
			}
		})
	}
}

func TestMCPToolErrorStatusParity(t *testing.T) {
	for _, hint := range []string{"meta", "status", "statusCode"} {
		for _, tc := range []struct {
			status any
			want   string
		}{
			{401, "auth_error"}, {403, "denied"}, {408, "timeout"}, {504, "timeout"}, {429, "rate_limit"}, {404, "invalid_request"}, {500, "api_error"}, {nil, "api_error"}, {"429", "api_error"},
		} {
			t.Run(fmt.Sprintf("%s/%v", hint, tc.status), func(t *testing.T) {
				s := workspaceServer("status-test", nil)
				r := wrap(t, s, Options{})
				w, _ := instances.get(s)
				result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "private error text"}}}
				if hint == "meta" {
					result.Meta = mcp.Meta{"com.rudderstack/http_status": tc.status}
				} else {
					result.StructuredContent = map[string]any{hint: tc.status}
				}
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return result, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "tool", Arguments: json.RawMessage(`{}`)}})
				if err != nil {
					t.Fatal(err)
				}
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

func TestMCPToolErrorClassifier(t *testing.T) {
	for _, kind := range []string{"rate_limit", "timeout", "cancelled", "denied", "auth_error", "invalid_request", "connection_error", "api_error", "unsupported", "", "panic"} {
		t.Run(kind, func(t *testing.T) {
			s := workspaceServer("classifier-test", nil)
			result := &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"status": 401}}
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
			w, _ := instances.get(s)
			invoke := func() {
				_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return result, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "tool", Arguments: json.RawMessage(`{}`)}})
				if err != nil {
					t.Fatal(err)
				}
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

func TestMCPStatelessClientEveryRequestType(t *testing.T) {
	for _, source := range []string{"meta", "resolver"} {
		for _, method := range []string{"ping", "tools/list", "resources/list", "resources/templates/list", "prompts/list", "resources/read", "prompts/get"} {
			t.Run(source+"/"+method, func(t *testing.T) {
				s := workspaceServer("client-types", nil)
				resolved := 0
				wrap(t, s, Options{ClientInfo: func(context.Context, *auth.TokenInfo) (string, string) { resolved++; return "client", "1" }})
				w, _ := instances.get(s)
				meta := mcp.Meta{}
				if source == "meta" {
					meta[metaClientInfo] = map[string]any{"name": "client", "version": "1"}
				}
				var req mcp.Request
				switch method {
				case "resources/read":
					req = &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "docs://readme", Meta: meta}}
				case "prompts/get":
					req = &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Name: "prompt", Meta: meta}}
				case "ping":
					req = &mcp.ServerRequest[*mcp.PingParams]{Params: &mcp.PingParams{Meta: meta}}
				default:
					req = &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{Meta: meta}}
				}
				h := w.before(context.Background(), method, req)
				if h.rc.base["client_name"] != "client" || h.rc.base["client_version"] != "1" {
					t.Errorf("client = %v", h.rc.base)
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

func TestMCPClientInfoFallback(t *testing.T) {
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
				r := wrap(t, s, Options{ClientInfo: func(context.Context, *auth.TokenInfo) (string, string) {
					resolved++
					return "resolved-client", "2"
				}})
				meta := mcp.Meta{metaClientInfo: tc.info}

				if session {
					cs := connect(t, s, "2025-06-18", nil)
					callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, meta)
				} else {
					w, _ := instances.get(s)
					_, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
						return &mcp.CallToolResult{}, nil
					})(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(`{"workspace_id":"w"}`), Meta: meta}})
					if err != nil {
						t.Fatal(err)
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

func TestMCPInvalidArgumentsWithoutToolsList(t *testing.T) {
	for _, inferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("inferred=%v", inferred), func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: t.Name(), Version: "1.0.0"}, &mcp.ServerOptions{PageSize: 1})
			if inferred {
				mcp.AddTool(s, &mcp.Tool{Name: "tool"}, func(context.Context, *mcp.CallToolRequest, struct {
					WorkspaceID string `json:"workspace_id"`
				}) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{IsError: true}, nil, nil
				})
			} else {
				s.AddTool(&mcp.Tool{Name: "tool", InputSchema: map[string]any{
					"type": "object", "required": []string{"workspace_id"},
					"properties": map[string]any{"workspace_id": map[string]any{"type": "string"}},
				}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{IsError: true}, nil
				})
			}
			// A second tool proves learning does not depend on listing page size.
			s.AddTool(&mcp.Tool{Name: "first", InputSchema: map[string]any{"type": "object"}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{}, nil
				})
			r := wrap(t, s, Options{})
			if got := len(registeredTools(s)); got != 2 {
				t.Fatalf("registered tools = %d, want 2", got)
			}
			cs := connect(t, s, "2025-06-18", nil)
			callTool(t, cs, "tool", map[string]any{"workspace_id": map[string]any{"secret": "private-value"}}, nil)
			p := r.props(t, "mcp_tool_called")
			if p["error_type"] != "invalid_request" {
				t.Errorf("error_type = %v", p["error_type"])
			}
			if got, _ := p["invalid_arguments"].([]string); !slices.Equal(got, []string{"workspace_id"}) {
				t.Errorf("invalid_arguments = %v", p["invalid_arguments"])
			}
			if strings.Contains(mustJSON(r.all()), "private-value") {
				t.Error("argument value leaked")
			}
		})
	}
}

func TestMCPInvalidArgumentsWithErrorClassification(t *testing.T) {
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

				result := &mcp.CallToolResult{IsError: tc.isError, StructuredContent: map[string]any{"status": tc.status}}
				s.AddTool(&mcp.Tool{Name: "tool", InputSchema: map[string]any{
					"type": "object", "required": []string{"workspace_id"},
					"properties": map[string]any{"workspace_id": map[string]any{"type": "string"}},
				}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return result, nil })
				r := wrap(t, s, Options{ClassifyToolError: func(string, *mcp.CallToolResult) string { return tc.classified }})
				cs := connect(t, s, "2025-06-18", nil)

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

func TestMCPCallbackPanicsReported(t *testing.T) {
	for _, where := range []string{"Options.ClassifyToolError", "Options.ClientInfo"} {
		t.Run(where, func(t *testing.T) {
			s := workspaceServer("callback-panic", nil)
			opts := Options{}
			if where == "Options.ClassifyToolError" {
				opts.ClassifyToolError = func(string, *mcp.CallToolResult) string { panic("callback failed") }
			} else {
				opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) { panic("callback failed") }
			}
			r := wrap(t, s, opts)
			seen := &seenErrors{}
			setupAI(t, WithOnError(seen.hook))
			w, _ := instances.get(s)
			result := &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"status": 401}}
			gotResult, err := w.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) { return result, nil })(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "tool", Arguments: json.RawMessage(`{}`)}})
			if err != nil || gotResult != result {
				t.Fatalf("result = %v, err = %v", gotResult, err)
			}
			p := r.props(t, "mcp_tool_called")
			if p["error_type"] != "auth_error" {
				t.Fatalf("fallback = %v", p)
			}
			if where == "Options.ClientInfo" {
				if _, ok := p["client_name"]; ok {
					t.Fatalf("client fallback = %v", p)
				}
			}
			safely(func() { panic("SDK failure") })
			got := seen.all()
			if len(got) != 2 || got[0] != (ErrorInfo{Kind: KindUserCallback, Where: where}) || got[1].Kind != KindInternal {
				t.Fatalf("hook saw %+v", got)
			}
		})
	}
}
