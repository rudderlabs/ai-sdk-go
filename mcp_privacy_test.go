package rudderstackai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestMCPPrivacyRegisteredNames(t *testing.T) {
	s := workspaceServer("privacy-names", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	tools := []mcpbridge.Tool{{Name: "registered", Schema: map[string]any{"properties": map[string]any{"kept": map[string]any{"type": "string"}}, "additionalProperties": false}}}
	for _, name := range []string{"registered", "jane.doe@acme.com"} {
		rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools})
		rc.ToolCall(name, map[string]json.RawMessage{"kept": json.RawMessage(`7`), "jane.doe@acme.com": json.RawMessage(`1`), "zzz-private": json.RawMessage(`2`)}, false, nil, "")
		rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"isError": true}})
		p := r.named("mcp_tool_called")[len(r.named("mcp_tool_called"))-1].Properties
		wantName, wantKeys := "registered", []string{"unknown", "kept"}
		if name != "registered" {
			wantName, wantKeys = "unknown", []string{"unknown"}
		}
		if p["tool_name"] != wantName || !slices.Equal(p["argument_keys"].([]string), wantKeys) {
			t.Errorf("names = %v", p)
		}
		if name == "registered" && !slices.Equal(p["invalid_arguments"].([]string), []string{"unknown", "kept"}) {
			t.Errorf("invalid = %v", p)
		}
	}
}

func TestMCPPrivacyRequestIDs(t *testing.T) {
	for _, id := range []string{"SELECT email FROM customers", "req-1", "7", strings.Repeat("a", 65), "", "line\n"} {
		t.Run(id, func(t *testing.T) {
			s := workspaceServer("privacy-id", nil)
			r := wrap(t, s, Options{})
			w, _ := instances.get(s)
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: id})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
			w.Elicited(rc, mcpbridge.Elicitation{Action: "accept"})
			rc.Finished("resources/read", "readme", 0, "")
			for _, event := range []string{"mcp_tool_called", "mcp_elicitation_completed", "mcp_resource_read"} {
				got, present := r.props(t, event)["request_id"]
				want := id
				if id == "" {
					want = "unknown"
				} else if !safeRPCString.MatchString(id) {
					sum := sha256.Sum256([]byte(id))
					want = "sha256:" + hex.EncodeToString(sum[:])[:16]
				}
				if !present || got != want {
					t.Errorf("%s id = %v, want %q", event, got, want)
				}
			}
		})
	}
}

func TestMCPPrivacyClientFields(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"\u0007bad\n  name", "bad name"}, {strings.Repeat("é", 100), strings.Repeat("é", 64)}, {"\u0000  \t", ""}, {" a\u0085b\u2003 c ", "ab c"}} {
		t.Run(tc.value, func(t *testing.T) {
			s := workspaceServer("privacy-client", nil)
			r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) {
				return Identity{UserID: "u", OAuthClientID: "app"}, nil
			}})
			w, _ := instances.get(s)
			client := &mcpbridge.Client{Name: tc.value, Title: tc.value, Version: tc.value}
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: client})
			rc.Initialized(client, nil, "")
			rc = w.begin(mcpbridge.Request{Ctx: context.Background()})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
			for _, event := range []string{"mcp_client_seen", "mcp_tool_called"} {
				for _, key := range []string{"client_name", "client_version", "client_title"} {
					if event == "mcp_tool_called" && key == "client_title" {
						continue
					}
					got, ok := r.props(t, event)[key]
					if tc.want == "" {
						if ok {
							t.Errorf("%s %s = %v", event, key, got)
						}
					} else if got != tc.want {
						t.Errorf("%s %s = %v", event, key, got)
					}
				}
			}
		})
	}
}

func TestMCPPrivacyCallerCounters(t *testing.T) {
	for _, scope := range []string{"session", "interaction", "connection"} {
		t.Run(scope, func(t *testing.T) {
			s := workspaceServer("privacy-counter", nil)
			r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
				return ctx.Value(privacyIdentityKey{}).(Identity), nil
			}})
			w, _ := instances.get(s)
			ids := []Identity{{AccountID: "a", UserID: "one"}, {AccountID: "a", UserID: "two"}, {AccountID: "b", UserID: "one"}, {AccountID: "a", UserID: "one"}}
			for i, id := range ids {
				req := mcpbridge.Request{Ctx: context.WithValue(context.Background(), privacyIdentityKey{}, id)}
				switch scope {
				case "session":
					req.SessionID = "shared"
				case "interaction":
					req.Meta = map[string]any{metaInteractionID: "shared"}
				case "connection":
					req.Connection = "shared"
				}
				rc := w.begin(req)
				rc.ToolCall("list_destinations", nil, false, nil, "")
				rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
				p := r.named("mcp_tool_called")[i].Properties
				want := 0
				if i == 3 {
					want = 1
				}
				if p["call_index"] != want || p["retry_count"] != want {
					t.Errorf("caller %v = %v", id, p)
				}
			}
		})
	}
}

type privacyIdentityKey struct{}

func TestMCPPrivacyRegistryAtCallTime(t *testing.T) {
	s := workspaceServer("privacy-registry", nil)
	r := wrap(t, s, Options{})
	cs := connect(t, s, "2025-06-18", nil)
	s.RemoveTools("list_destinations")
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_destinations"})
	if p := r.named("mcp_tool_called")[0].Properties; p["tool_name"] != "unknown" {
		t.Errorf("removed tool = %v", p)
	}
	s.AddTool(&mcp.Tool{Name: "late", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"kept": map[string]any{"type": "string"}}}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "late", Arguments: map[string]any{"kept": "ok"}})
	if p := r.named("mcp_tool_called")[1].Properties; p["tool_name"] != "late" || !slices.Equal(p["argument_keys"].([]string), []string{"kept"}) {
		t.Errorf("late tool = %v", p)
	}
}

func TestMCPPrivacyLongCallerIDs(t *testing.T) {
	s := workspaceServer("privacy-long-callers", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		return ctx.Value(privacyIdentityKey{}).(Identity), nil
	}})
	w, _ := instances.get(s)
	for i, user := range []string{strings.Repeat("u", 300) + "one", strings.Repeat("u", 300) + "two"} {
		rc := w.begin(mcpbridge.Request{Ctx: context.WithValue(context.Background(), privacyIdentityKey{}, Identity{UserID: user}), SessionID: "shared"})
		rc.ToolCall("list_destinations", nil, false, nil, "")
		rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
		if p := r.named("mcp_tool_called")[i].Properties; p["call_index"] != 0 {
			t.Errorf("long caller shared state: %v", p)
		}
	}
}

func TestMCPPrivacyDeclaredCopies(t *testing.T) {
	s := workspaceServer("privacy-declared", nil)
	r := wrap(t, s, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "performed", Properties: func(map[string]any, any) map[string]any {
		return map[string]any{"argument_keys": []string{"jane.doe@acme.com", "workspace_id", "other", "workspace_id"}, "invalid_arguments": []string{"other", "workspace_id", "other"}, "request_id": "SELECT email FROM customers", "client_title": "\u0007bad\n  name"}
	}}}}})
	w, _ := instances.get(s)
	tools := []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"properties": map[string]any{"workspace_id": map[string]any{}}}}}
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools, RequestID: int64(7)})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	p := r.props(t, "performed")
	for _, key := range []string{"argument_keys", "invalid_arguments"} {
		if !slices.Equal(p[key].([]string), []string{"unknown", "workspace_id"}) {
			t.Errorf("%s = %v", key, p[key])
		}
	}
	if p["request_id"] != "7" {
		t.Errorf("forged id kept: %v", p)
	}
	if p["client_title"] != "bad name" {
		t.Errorf("client title copied: %v", p)
	}
}

func TestMCPPrivacyPendingInputs(t *testing.T) {
	s := workspaceServer("privacy-inputs", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		return ctx.Value(privacyIdentityKey{}).(Identity), nil
	}})
	w, _ := instances.get(s)
	begin := func(user string) *reqCtx {
		return w.begin(mcpbridge.Request{Ctx: context.WithValue(context.Background(), privacyIdentityKey{}, Identity{AccountID: "a", UserID: user}), SessionID: "shared", RequestID: "req-1"})
	}
	first := begin("one")
	first.ToolCall("list_destinations", nil, false, nil, "")
	first.ToolDone(mcpbridge.Outcome{InputRequired: true, InputKeys: []string{"same"}, RequestState: "same-state"})
	other := begin("two")
	other.ToolCall("list_destinations", nil, false, []mcpbridge.Answer{{Key: "same", Action: "accept"}}, "same-state")
	other.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	original := begin("one")
	original.ToolCall("list_destinations", nil, false, []mcpbridge.Answer{{Key: "same", Action: "accept"}}, "same-state")
	original.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	elicited := r.named("mcp_elicitation_completed")
	if _, ok := elicited[0].Properties["duration_ms"]; ok {
		t.Error("other caller consumed pending input")
	}
	if _, ok := elicited[1].Properties["duration_ms"]; !ok {
		t.Error("original caller lost pending input")
	}
	for _, event := range r.named("mcp_tool_called") {
		if event.Properties["call_index"] != 0 {
			t.Errorf("pending position = %v", event.Properties)
		}
	}
}
