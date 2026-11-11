package rudderstackai

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestSafeRequestIDExactValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   any
		want string
	}{
		{"absent", nil, ""},
		{"int", int64(7), "7"},
		{"whole float", float64(7), "7"},
		{"negative zero", math.Copysign(0, -1), "0"},
		{"negative", int64(-12), "-12"},
		{"largest safe", int64(9007199254740991), "9007199254740991"},
		{"above safe", int64(9007199254740992), "unknown"},
		{"1e21 float", 1e21, "unknown"},
		{"1e21 number", json.Number("1e21"), "unknown"},
		{"7.0 number", json.Number("7.0"), "7"},
		{"fraction", 1.5, "unknown"},
		{"nan", math.NaN(), "unknown"},
		{"bool", true, "unknown"},
		{"object", map[string]any{"a": 1}, "unknown"},
		{"empty string", "", "unknown"},
		{"short string", "req-1", "sha256:9456bdfa12ea7695"},
		{"sensitive-looking", "customer:ssn:123-45-6789", hashedRequestID("customer:ssn:123-45-6789")},
		{"numeric string", "7", "sha256:7902699be42c8a8e"},
		{"sql", "SELECT email FROM customers", "sha256:409abf7beb528a87"},
		{"long", strings.Repeat("a", 65), hashedRequestID(strings.Repeat("a", 65))},
	} {
		if got := safeRequestID(tc.id); got != tc.want {
			t.Errorf("%s: safeRequestID(%v) = %q, want %q", tc.name, tc.id, got, tc.want)
		}
	}
}

// 7 and "7" are different requests: concurrent calls each send their own event.
func TestNumericAndStringIDsAreSeparateRequests(t *testing.T) {
	s := workspaceServer("hardening-ids", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	a := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: int64(7)})
	b := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: "7"})
	a.ToolCall("list_destinations", nil, false, nil, "")
	b.ToolCall("list_destinations", nil, false, nil, "")
	b.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	a.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	if got := len(r.named("mcp_tool_called")); got != 2 {
		t.Errorf("events = %d, want 2", got)
	}
}

func TestProtocolVersionExactValues(t *testing.T) {
	for _, tc := range []struct {
		name, meta, header, want string
		fromSession              bool
	}{
		{"valid meta", "2026-07-28", "2025-06-18", "2026-07-28", false},
		{"evil meta", "evil", "2025-06-18", "unknown", false},
		{"header fallback", "", "2025-06-18", "2025-06-18", false},
		{"header fallback beside an unknown caller", "", "2025-06-18", "2025-06-18", true},
		{"evil header", "", "evil", "unknown", false},
		{"trailing newline", "2025-06-18\n", "", "unknown", false},
		{"none", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer("hardening-version", nil)
			r := wrap(t, s, Options{})
			w, _ := instances.get(s)
			req := mcpbridge.Request{Ctx: context.Background(), ProtocolVersion: tc.header}
			if tc.meta != "" {
				req.Meta = map[string]any{metaProtocolVersion: tc.meta}
			}
			if tc.fromSession {
				req.ClientFor = func(string, string, string) (*mcpbridge.Client, string) { return nil, "" }
			}
			rc := w.begin(req)
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
			got, present := r.props(t, "mcp_tool_called")["protocol_version"]
			if present != (tc.want != "") || (present && got != tc.want) {
				t.Errorf("protocol_version = %v (present %v), want %q", got, present, tc.want)
			}
		})
	}
}

// Sorted first (as argument names are), then unrecognized names become one "unknown".
func TestCapabilityNamesAreAnAllowlist(t *testing.T) {
	got := capabilityNames(map[string]any{"sampling": map[string]any{}, "evil-vendor": 1, "roots": map[string]any{}, "other": 1})
	if want := []string{"unknown", "roots", "sampling"}; !slices.Equal(got, want) {
		t.Errorf("capabilityNames = %q, want %q", got, want)
	}
}

func TestArgumentNamesSortedThenMappedThenDeduplicated(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"a": map[string]any{"type": "string"}, "z": map[string]any{"type": "string"}},
	}
	s := workspaceServer("hardening-args", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	tools := []mcpbridge.Tool{{Name: "probe", Schema: schema}}
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools})
	rc.ToolCall("probe", map[string]json.RawMessage{"z": json.RawMessage(`1`), "a": json.RawMessage(`2`), "evil": json.RawMessage(`3`)}, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"isError": true}})
	p := r.props(t, "mcp_tool_called")
	want := []string{"a", "unknown", "z"}
	// a and z fail the string type; evil fails additionalProperties.
	if !slices.Equal(p["argument_keys"].([]string), want) || !slices.Equal(p["invalid_arguments"].([]string), []string{"a", "unknown", "z"}) {
		t.Errorf("argument_keys = %v, invalid_arguments = %v", p["argument_keys"], p["invalid_arguments"])
	}

	// Registered names are exact: a 70-code-point name is neither cut nor merged with another.
	long1, long2 := strings.Repeat("x", 70)+"1", strings.Repeat("x", 70)+"2"
	if got := registeredArgumentNames(map[string]any{"properties": map[string]any{long1: 1, long2: 1}}, []string{long2, long1}); !slices.Equal(got, []string{long1, long2}) {
		t.Errorf("long names = %q", got)
	}
}

func TestRegistrySnapshotIsNeverSticky(t *testing.T) {
	s := workspaceServer("hardening-registry", nil)
	r := wrap(t, s, Options{})
	cs := connect(t, s, "2025-06-18", nil)
	// Registered after the wrap and never listed by a client.
	mcp.AddTool(s, &mcp.Tool{Name: "late", Description: "Late."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return nil, nil, nil
	})
	callTool(t, cs, "late", nil, nil)
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	s.RemoveTools("late")
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "late"})
	events := r.named("mcp_tool_called")
	if len(events) != 2 || events[0].Properties["tool_name"] != "late" || events[1].Properties["tool_name"] != "unknown" {
		t.Errorf("tool_name = %v", r.names())
	}
}

func TestPromptNamesAreRegistered(t *testing.T) {
	s := workspaceServer("hardening-prompts", nil)
	s.AddPrompt(&mcp.Prompt{Name: "audit"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	r := wrap(t, s, Options{})
	cs := connect(t, s, "2025-06-18", nil)
	_, _ = cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "audit"})
	_, _ = cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "jane.doe@acme.com"})
	events := r.named("mcp_prompt_used")
	if len(events) != 2 || events[0].Properties["prompt_name"] != "audit" || events[1].Properties["prompt_name"] != "unknown" {
		t.Errorf("prompt events = %v", mustJSON(events))
	}
}

func TestDeclaredPropertiesAreCleanedWhateverTheirSpelling(t *testing.T) {
	s := workspaceServer("hardening-declared", nil)
	r := wrap(t, s, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{"probe": {Event: "performed", Properties: func(map[string]any, any) map[string]any {
		return map[string]any{
			"argumentKeys": []string{"z", "evil", "a"}, "Invalid-Arguments": "evil", "REQUEST_ID": "forged", "clientTitle": "\u0007bad\n  title",
			"client-version": "  1.0 \t beta ", "toolName": "forged", "clientName": "\u0000",
		}
	}}}}})
	w, _ := instances.get(s)
	tools := []mcpbridge.Tool{{Name: "probe", Schema: map[string]any{"properties": map[string]any{"a": 1, "z": 1}}}}
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools, RequestID: "real-1"})
	rc.ToolCall("probe", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	p := r.props(t, "performed")
	if !slices.Equal(p["argument_keys"].([]string), []string{"a", "unknown", "z"}) || !slices.Equal(p["invalid_arguments"].([]string), []string{"unknown"}) {
		t.Errorf("lists = %v / %v", p["argument_keys"], p["invalid_arguments"])
	}
	if p["request_id"] != "sha256:9773879a094d490e" || p["client_title"] != "bad title" || p["client_version"] != "1.0 beta" || p["tool_name"] != "probe" {
		t.Errorf("properties = %v", p)
	}
	if _, ok := p["client_name"]; ok {
		t.Errorf("empty client_name kept: %v", p)
	}
	for k := range p {
		if k != strings.ToLower(k) || strings.ContainsAny(k, "-A") {
			t.Errorf("non-canonical property %q", k)
		}
	}
}

// Identifiers are never cut or trimmed in state keys: callers equal for their first 300
// characters keep separate counters and client windows.
func TestLongIdentitiesKeepSeparateState(t *testing.T) {
	prefix := strings.Repeat("u", 300)
	s := workspaceServer("hardening-long", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		return ctx.Value(privacyIdentityKey{}).(Identity), nil
	}})
	w, _ := instances.get(s)
	for _, tc := range []struct {
		name string
		req  func(Identity) mcpbridge.Request
	}{
		{"session", func(id Identity) mcpbridge.Request { return mcpbridge.Request{SessionID: "shared"} }},
		{"interaction", func(id Identity) mcpbridge.Request {
			return mcpbridge.Request{Meta: map[string]any{metaInteractionID: prefix + "x"}}
		}},
		{"connection", func(id Identity) mcpbridge.Request { return mcpbridge.Request{Connection: "c1"} }},
	} {
		r.reset()
		for _, user := range []string{prefix + "one", prefix + "two"} {
			req := tc.req(Identity{})
			req.Ctx = context.WithValue(context.Background(), privacyIdentityKey{}, Identity{UserID: user, AccountID: prefix})
			rc := w.begin(req)
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
		}
		for i, e := range r.named("mcp_tool_called") {
			if e.Properties["call_index"] != 0 {
				t.Errorf("%s: call %d shares state: %v", tc.name, i, e.Properties)
			}
		}
	}

	// Two long users are two mcp_client_seen windows; one user under two accounts also.
	client := &mcpbridge.Client{Name: "agent"}
	r.reset()
	w2 := stateFor("hardening-long")
	w2.Lock()
	w2.clientSeen.clear()
	w2.Unlock()
	for _, id := range []Identity{{UserID: prefix + "one"}, {UserID: prefix + "two"}, {UserID: "same", AccountID: "a1"}, {UserID: "same", AccountID: "a2"}} {
		rc := w.begin(mcpbridge.Request{Ctx: context.WithValue(context.Background(), privacyIdentityKey{}, id), InitializeClient: client})
		rc.Initialized(client, nil, "2025-06-18")
	}
	if got := len(r.named("mcp_client_seen")); got != 4 {
		t.Errorf("mcp_client_seen = %d, want 4", got)
	}
}

// A caller identified differently never inherits another caller's session client.
func TestSessionClientIsPerCaller(t *testing.T) {
	s := workspaceServer("hardening-session-client", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		call, _ := ToolCallFromContext(ctx)
		var args struct {
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		if args.WorkspaceID == "anon" {
			return Identity{}, nil
		}
		return Identity{UserID: args.WorkspaceID}, nil
	}})
	cs := connect(t, s, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "anon"}, nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "bob"}, nil)
	events := r.named("mcp_tool_called")
	if events[0].Properties["client_name"] != "test-client" {
		t.Errorf("initializing caller lost its client: %v", events[0].Properties)
	}
	if name, ok := events[1].Properties["client_name"]; ok {
		t.Errorf("another caller inherited client %v", name)
	}
}

func TestPendingInputsAreScopedAndExpire(t *testing.T) {
	s := workspaceServer("hardening-pending", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	begin := func(session string) *reqCtx {
		return w.begin(mcpbridge.Request{Ctx: context.Background(), SessionID: session, RequestID: "req-" + session})
	}
	answer := []mcpbridge.Answer{{Key: "k", Action: "accept"}}
	for _, state := range []string{"", "st"} {
		r.reset()
		first := begin("s1")
		first.ToolCall("list_destinations", nil, false, nil, "")
		first.ToolDone(mcpbridge.Outcome{InputRequired: true, InputKeys: []string{"k"}, RequestState: state})

		other := begin("s2")
		other.ToolCall("list_destinations", nil, false, answer, state)
		other.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
		original := begin("s1")
		original.ToolCall("list_destinations", nil, false, answer, state)
		original.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})

		elicited := r.named("mcp_elicitation_completed")
		if _, ok := elicited[0].Properties["duration_ms"]; ok || elicited[0].Properties["request_id"] != nil {
			t.Errorf("state %q: session 2 consumed session 1's continuation: %v", state, elicited[0].Properties)
		}
		if elicited[1].Properties["request_id"] != "sha256:1452c9582cee073d" {
			t.Errorf("state %q: session 1 lost its continuation: %v", state, elicited[1].Properties)
		}
	}

	r.reset()
	first := begin("s3")
	first.ToolCall("list_destinations", nil, false, nil, "")
	first.ToolDone(mcpbridge.Outcome{InputRequired: true, InputKeys: []string{"k"}})
	key := first.inputKey("k", "")
	first.st.Lock()
	pending, _ := first.st.inputRequired.get(key)
	pending.start = time.Now().Add(-pendingInputTTL - time.Minute)
	first.st.inputRequired.set(key, pending)
	first.st.Unlock()
	retry := begin("s3")
	retry.ToolCall("list_destinations", nil, false, answer, "")
	retry.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	p := r.props(t, "mcp_elicitation_completed")
	if _, ok := p["duration_ms"]; ok || p["request_id"] != nil {
		t.Errorf("expired continuation reused: %v", p)
	}
	if got := r.named("mcp_tool_called")[0].Properties["call_index"]; got != 1 {
		t.Errorf("expired continuation kept its position: call_index = %v", got)
	}
}

// Stateless callers in one account that differ only by anonymous id keep separate state.
func TestAnonymousCallersKeepSeparateState(t *testing.T) {
	s := workspaceServer("hardening-anon", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		return ctx.Value(privacyIdentityKey{}).(Identity), nil
	}})
	w, _ := instances.get(s)
	begin := func(anon, requestID string) *reqCtx {
		ctx := context.WithValue(context.Background(), privacyIdentityKey{}, Identity{AccountID: "acct", AnonymousID: anon})
		return w.begin(mcpbridge.Request{Ctx: ctx, RequestID: requestID})
	}
	answer := []mcpbridge.Answer{{Key: "k", Action: "accept"}}
	a := begin("anon-a", "req-a")
	a.ToolCall("list_destinations", nil, false, nil, "")
	a.ToolDone(mcpbridge.Outcome{InputRequired: true, InputKeys: []string{"k"}})

	b := begin("anon-b", "req-b")
	b.ToolCall("list_destinations", nil, false, answer, "")
	b.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})

	elicited := r.named("mcp_elicitation_completed")
	if _, ok := elicited[0].Properties["duration_ms"]; ok || elicited[0].Properties["request_id"] == safeRequestID("req-a") {
		t.Errorf("caller B consumed caller A's continuation: %v", elicited[0].Properties)
	}
}

// A known user keeps one state key while the anonymous id changes.
func TestKnownUserSharesStateAcrossAnonymousIDs(t *testing.T) {
	s := workspaceServer("hardening-known-user", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context, _ *auth.TokenInfo) (Identity, error) {
		return ctx.Value(privacyIdentityKey{}).(Identity), nil
	}})
	w, _ := instances.get(s)
	client := &mcpbridge.Client{Name: "agent"}
	for _, anon := range []string{"anon-a", "anon-b"} {
		ctx := context.WithValue(context.Background(), privacyIdentityKey{}, Identity{UserID: "u1", AccountID: "acct", AnonymousID: anon})
		rc := w.begin(mcpbridge.Request{Ctx: ctx, InitializeClient: client, Meta: map[string]any{metaInteractionID: "shared"}})
		rc.Initialized(client, nil, "2025-06-18")
		rc.ToolCall("list_destinations", nil, false, nil, "")
		rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{}})
	}
	if got := len(r.named("mcp_client_seen")); got != 1 {
		t.Errorf("mcp_client_seen = %d, want 1", got)
	}
	for i, e := range r.named("mcp_tool_called") {
		if e.Properties["call_index"] != i {
			t.Errorf("call %d: call_index = %v, want %d", i, e.Properties["call_index"], i)
		}
	}
}

// A title-only clientInfo keeps its title.
func TestTitleOnlyClientKeepsTitle(t *testing.T) {
	s := workspaceServer("hardening-title", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	client := &mcpbridge.Client{Title: "Only A Title"}
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: client})
	rc.Initialized(client, nil, "2025-06-18")
	if got := r.props(t, "mcp_client_seen")["client_title"]; got != "Only A Title" {
		t.Errorf("client_title = %v", got)
	}
}
