package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestClientSeenOncePerClientAtInitialize(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{Transport: TransportStdio})
	connect(t, server, "2025-06-18", &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "accept"}, nil
	}})
	connect(t, server, "2025-06-18", nil)
	p := r.props(t, "mcp_client_seen")
	want := map[string]any{
		"server_name": "workspace-mcp", "server_version": "1.4.0", "client_name": "test-client",
		"client_title": "Test Client", "client_version": "1.2.3", "protocol_version": "2025-06-18",
		"transport": "stdio", "auth_method": "none",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	if caps := p["client_capabilities"].([]string); !slices.Contains(caps, "elicitation") {
		t.Errorf("client_capabilities = %v", caps)
	}
	if _, ok := p["mcp_session_id"]; ok {
		t.Error("in-memory sessions have no session ID")
	}
	m := r.named("mcp_client_seen")[0]
	if m.AnonymousId != "ai_system" || m.UserId != "" {
		t.Errorf("identity = %q/%q", m.UserId, m.AnonymousId)
	}
	wl := m.Context.Extra["wrapperLibrary"].(map[string]any)
	if wl["name"] != "ai-sdk-go" || wl["specVersion"] != "0.3.0" || wl["integration"] != "mcp" || wl["version"] != Version {
		t.Errorf("wrapperLibrary = %v", wl)
	}
}

func TestClientSeenAgainAfterWindow(t *testing.T) {
	server := workspaceServer("window-mcp", nil)
	r := wrap(t, server, Options{ClientSeenWindow: time.Millisecond})
	connect(t, server, "2025-06-18", nil)
	time.Sleep(5 * time.Millisecond)
	connect(t, server, "2025-06-18", nil)
	if got := len(r.named("mcp_client_seen")); got != 2 {
		t.Fatalf("mcp_client_seen = %d, want 2", got)
	}
}

func TestToolCalledShapesNeverValues(t *testing.T) {
	seen := &handlerSeen{}
	server := workspaceServer("workspace-mcp", seen)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "ws_private_42", "page_size": 5},
		mcp.Meta{"progressToken": "p1"})
	p := r.props(t, "mcp_tool_called")
	if p["tool_name"] != "list_destinations" || p["tool_status"] != "success" || p["is_error_result"] != false {
		t.Errorf("status fields: %v", p)
	}
	if got := p["argument_keys"].([]string); !slices.Equal(got, []string{"page_size", "workspace_id"}) {
		t.Errorf("argument_keys = %v", got)
	}
	if p["progress_requested"] != true || p["result_paginated"] != true {
		t.Errorf("progress/paginated: %v", p)
	}
	if got := p["result_content_types"].([]string); !slices.Equal(got, []string{"text", "structured"}) {
		t.Errorf("result_content_types = %v", got)
	}
	size := p["result_size_bytes"].(int)
	if size <= 0 || p["result_tokens_estimate"] != size/4 {
		t.Errorf("sizes: %v", p)
	}
	if p["request_id"] == "unknown" || p["request_id"] == "" {
		t.Errorf("request_id = %v", p["request_id"])
	}
	if p["protocol_version"] != "2025-06-18" || p["client_name"] != "test-client" || p["client_version"] != "1.2.3" {
		t.Errorf("client fields: %v", p)
	}
	if _, ok := p["duration_ms"].(int64); !ok {
		t.Errorf("duration_ms = %T", p["duration_ms"])
	}
	for _, k := range []string{"call_index", "retry_count", "ms_since_previous_call", "trace_id"} {
		if _, ok := p[k]; ok {
			t.Errorf("%s set without a scope or trace", k)
		}
	}
	if all := mustJSON(r.all()); strings.Contains(all, "ws_private_42") || strings.Contains(all, "secret-destination") {
		t.Error("arguments or results leaked into events")
	}
}

func TestToolCalledJoinsCallerAndCountsInScope(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	meta := mcp.Meta{metaInteractionID: "int_1", metaTraceparent: traceparent, "other": "never copied"}
	args := map[string]any{"workspace_id": "w"}
	callTool(t, cs, "list_destinations", args, meta)
	callTool(t, cs, "list_destinations", args, meta)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "x"}, meta)
	callTool(t, cs, "list_destinations", args, mcp.Meta{metaInteractionID: "int_2"})
	got := r.named("mcp_tool_called")
	type pos struct{ index, retry any }
	want := []pos{{0, 0}, {1, 1}, {2, 0}, {0, 0}}
	for i, m := range got {
		p := m.Properties
		if (pos{p["call_index"], p["retry_count"]}) != want[i] {
			t.Errorf("call %d: index/retry = %v/%v, want %v", i, p["call_index"], p["retry_count"], want[i])
		}
		if i > 0 && i < 3 {
			if _, ok := p["ms_since_previous_call"]; !ok {
				t.Errorf("call %d: no ms_since_previous_call", i)
			}
		}
	}
	p := got[0].Properties
	if p["interaction_id"] != "int_1" || p["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("join ids: %v", p)
	}
	if strings.Contains(mustJSON(r.all()), "never copied") {
		t.Error("other _meta copied")
	}
}

func TestTraceIDFallbackAndCaps(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	long := strings.Repeat("é", 300)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"},
		mcp.Meta{metaInteractionID: long, metaTraceID: "trace-7", metaTraceparent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01"})
	p := r.props(t, "mcp_tool_called")
	if p["trace_id"] != "trace-7" {
		t.Errorf("trace_id = %v", p["trace_id"])
	}
	if id := p["interaction_id"].(string); id != strings.Repeat("é", 256) {
		t.Errorf("interaction_id not capped to 256 code points: %d", len([]rune(id)))
	}
}

func TestToolErrorResult(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	res := callTool(t, cs, "fail", map[string]any{}, nil)
	if !res.IsError {
		t.Fatal("want an error result")
	}
	p := r.props(t, "mcp_tool_called")
	if p["tool_status"] != "error" || p["is_error_result"] != true || p["error_type"] != "api_error" {
		t.Errorf("error fields: %v", p)
	}
	if strings.Contains(mustJSON(r.all()), "boom") {
		t.Error("error text leaked")
	}
}

func TestInvalidArgumentsNamesNotValues(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, cs, "create_audience", map[string]any{"type": "sneaky-value", "extra": 1}, nil)
	if !res.IsError {
		t.Fatal("want a validation error result")
	}
	p := r.props(t, "mcp_tool_called")
	if p["error_type"] != "invalid_request" {
		t.Errorf("error_type = %v", p["error_type"])
	}
	if got := p["invalid_arguments"].([]string); !slices.Equal(got, []string{"name", "extra", "type"}) {
		t.Errorf("invalid_arguments = %v", got)
	}
	if strings.Contains(mustJSON(r.all()), "sneaky-value") {
		t.Error("argument value leaked")
	}
}

func TestCancelledCall(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "slow-mcp", Version: "1"}, nil)
	started := make(chan struct{})
	mcp.AddTool(server, &mcp.Tool{Name: "slow"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}}); err == nil {
		t.Fatal("want a cancelled call")
	}
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	if p["error_type"] != "cancelled" || p["tool_status"] != "error" {
		t.Errorf("cancel fields: %v", p)
	}
	if ms, ok := p["time_to_cancel_ms"].(int64); !ok || ms < 10 {
		t.Errorf("time_to_cancel_ms = %v", p["time_to_cancel_ms"])
	}
	if _, ok := p["duration_ms"]; ok {
		t.Error("duration_ms on a cancelled call")
	}
}

func TestProtocolErrorsMapToErrorType(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "raw-mcp", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "rate_limited", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: -32001, Message: "timed out"}
		})
	server.AddTool(&mcp.Tool{Name: "bad_params", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad"}
		})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	for _, name := range []string{"rate_limited", "bad_params", "missing_tool"} {
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name}); err == nil {
			t.Fatalf("%s: want a protocol error", name)
		}
	}
	var got []any
	for _, m := range r.named("mcp_tool_called") {
		got = append(got, m.Properties["error_type"])
	}
	if want := []any{"timeout", "invalid_request", "invalid_request"}; !slices.Equal(got, want) {
		t.Errorf("error types = %v, want %v", got, want)
	}
}

func TestRewrapReplacesOptions(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	first := wrap(t, server, Options{})
	second := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if len(first.all()) != 0 || len(second.named("mcp_tool_called")) != 1 {
		t.Errorf("first=%v second=%v", first.names(), second.names())
	}
}

func TestOptionsValidation(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := &recorder{}
	cases := map[string]Options{
		"no client":    {},
		"reserved ai":  {Analytics: r, EventRules: EventRules{Tools: map[string]ToolEventRule{"x": {Event: "ai_x"}}}},
		"reserved mcp": {Analytics: r, EventRules: EventRules{Tools: map[string]ToolEventRule{"x": {Event: "mcp_x"}}}},
		"empty event":  {Analytics: r, EventRules: EventRules{Tools: map[string]ToolEventRule{"x": {}}}},
		"window":       {Analytics: r, ClientSeenWindow: -time.Second},
		"transport":    {Analytics: r, Transport: "carrier-pigeon"},
	}
	for name, opts := range cases {
		if err := WrapMCPServer(server, opts); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := WrapMCPServer(nil, Options{Analytics: r}); err == nil {
		t.Error("nil server: want an error")
	}
}

func TestIdentifyTokenAndFailures(t *testing.T) {
	opts := &options{Options: Options{Identify: func(ctx context.Context, tok *auth.TokenInfo) (Identity, error) {
		return Identity{AccountID: tok.Extra["workspace"].(string), OAuthClientID: "mcpc_1"}, nil
	}}}
	tok := &auth.TokenInfo{UserID: "oauth|8812", Scopes: []string{"read"}, Extra: map[string]any{"workspace": "ws_1"}}
	id := opts.identity(context.Background(), tok)
	if id.UserID != "oauth|8812" || id.AccountID != "ws_1" || id.AuthMethod != "oauth" || !slices.Equal(id.TokenScopes, []string{"read"}) {
		t.Errorf("identity = %+v", id)
	}
	opts.Identify = func(context.Context, *auth.TokenInfo) (Identity, error) {
		return Identity{UserID: "x"}, errors.New("no")
	}
	if id := opts.identity(context.Background(), nil); id.UserID != "" || id.AuthMethod != "none" {
		t.Errorf("failed identify = %+v", id)
	}
	opts.Identify = func(context.Context, *auth.TokenInfo) (Identity, error) { panic("boom") }
	if id := opts.identity(context.Background(), tok); id.UserID != "oauth|8812" || id.AccountID != "" {
		t.Errorf("panicking identify = %+v", id)
	}
}

func TestIdentifyPanicKeepsServing(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { panic("boom") }})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

// broken is an analytics client that fails or panics on every call.
type broken struct{ panics bool }

func (b broken) Enqueue(analytics.Message) error {
	if b.panics {
		panic("analytics down")
	}
	return errors.New("analytics down")
}

func (broken) Close() error { return nil }

func TestAnalyticsFailuresNeverReachTheServer(t *testing.T) {
	for _, client := range []broken{{panics: true}, {}} {
		server := workspaceServer("workspace-mcp", nil)
		if err := WrapMCPServer(server, Options{Analytics: client, CapabilityRequests: &CapabilityRequestOptions{}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(resetMemory)
		cs := connect(t, server, "2025-06-18", nil)
		if res := callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil); res.IsError {
			t.Fatal("the tool call failed")
		}
		if res := callTool(t, cs, capabilityTool, map[string]any{"capability": "x"}, nil); res.IsError {
			t.Fatal("request_capability failed")
		}
	}
}

func TestRequestIDFallsBackOutsideTheSDK(t *testing.T) {
	if got := requestIDOf(context.Background()); got != "unknown" {
		t.Errorf("requestIDOf = %q", got)
	}
}

func TestServerImplFallback(t *testing.T) {
	name, version := serverImpl(mcp.NewServer(&mcp.Implementation{Name: "", Version: ""}, nil))
	if name != "unknown" || version != "" {
		t.Errorf("serverImpl = %q %q", name, version)
	}
}

func TestStrippedArgumentsReachHandler(t *testing.T) {
	seen := &handlerSeen{}
	server := workspaceServer("workspace-mcp", seen)
	r := wrap(t, server, Options{RequestContext: RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		schema := tool.InputSchema.(map[string]any)
		rc := schema["properties"].(map[string]any)[requestContext].(map[string]any)
		props := rc["properties"].(map[string]any)
		if _, ok := props["call_reason"]; ok {
			t.Error("call_reason offered without content capture")
		}
		if !slices.Equal(toStrings(props["intent"].(map[string]any)["enum"]), []string{"debug_delivery", "build_audience", "other"}) {
			t.Errorf("intent enum = %v", props["intent"])
		}
	}
	callTool(t, cs, "list_destinations", map[string]any{
		"workspace_id": "w",
		requestContext: map[string]any{"intent": "steal_data", "user_sentiment": "negative", "call_reason": "why"},
	}, nil)
	if strings.Contains(seen.last(), requestContext) {
		t.Errorf("handler saw request_context: %s", seen.last())
	}
	p := r.props(t, "mcp_tool_called")
	if p["intent"] != "other" || p["user_sentiment"] != "negative" {
		t.Errorf("context = %v", p)
	}
	if _, ok := p["call_reason"]; ok {
		t.Error("call_reason sent without content capture")
	}
	if got := p["argument_keys"].([]string); !slices.Equal(got, []string{"workspace_id"}) {
		t.Errorf("argument_keys = %v", got)
	}
}

func toStrings(v any) []string {
	var out []string
	_ = json.Unmarshal([]byte(mustJSON(v)), &out)
	return out
}

func TestCallReasonWithCaptureMaskedAndCapped(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{CaptureContent: true})
	cs := connect(t, server, "2025-06-18", nil)
	reason := "  Mail ann@example.com about it " + strings.Repeat("x", 300)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w", requestContext: map[string]any{"call_reason": reason, "intent": "anything"}}, nil)
	p := r.props(t, "mcp_tool_called")
	got := p["call_reason"].(string)
	if !strings.HasPrefix(got, "Mail [email] about it") || len([]rune(got)) != 200 {
		t.Errorf("call_reason = %q", got)
	}
	if _, ok := p["intent"]; ok {
		t.Error("intent sent without an intent list")
	}
}

func TestRequestContextDisabled(t *testing.T) {
	seen := &handlerSeen{}
	server := workspaceServer("workspace-mcp", seen)
	wrap(t, server, Options{RequestContext: RequestContextOptions{Disabled: true}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(tools), requestContext) {
		t.Error("request_context offered while disabled")
	}
	wrap(t, server, Options{RequestContext: RequestContextOptions{DisableUserSentiment: true}})
	tools, _ = cs.ListTools(context.Background(), nil)
	if strings.Contains(mustJSON(tools), requestContext) {
		t.Error("an empty request_context was offered")
	}
}

func TestNativeRequestContextLeftAlone(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "native-mcp", Version: "1"}, nil)
	var got string
	server.AddTool(&mcp.Tool{Name: "own", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{requestContext: map[string]any{"type": "string"}},
	}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		got = string(req.Params.Arguments)
		return &mcp.CallToolResult{}, nil
	})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if prop := tools.Tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)[requestContext]; mustJSON(prop) != `{"type":"string"}` {
		t.Errorf("schema changed: %v", prop)
	}
	callTool(t, cs, "own", map[string]any{requestContext: "mine"}, nil)
	if got != `{"request_context":"mine"}` {
		t.Errorf("handler got %s", got)
	}
	if got := r.props(t, "mcp_tool_called")["argument_keys"].([]string); !slices.Equal(got, []string{requestContext}) {
		t.Errorf("argument_keys = %v", got)
	}
}

func TestDeclaredToolEvent(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{
		"create_audience": {
			Event: "audience_created",
			Properties: func(args map[string]any, result any) map[string]any {
				return map[string]any{"audience_type": result.(map[string]any)["type"], "tool_name": "spoofed", "has_name": args["name"] != nil}
			},
			When: func(args map[string]any, _ any) bool { return args["name"] != "skip" },
		},
		"list_destinations": {Event: "destinations_listed"},
	}}})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "create_audience", map[string]any{"name": "vip", "type": "computed"}, mcp.Meta{metaInteractionID: "int_5"})
	callTool(t, cs, "create_audience", map[string]any{"name": "skip"}, nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	p := r.props(t, "audience_created")
	if p["audience_type"] != "computed" || p["tool_name"] != "create_audience" || p["tool_status"] != "success" || p["has_name"] != true || p["interaction_id"] != "int_5" {
		t.Errorf("declared event = %v", p)
	}
	if len(r.named("destinations_listed")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestDeclaredRulePanics(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{EventRules: EventRules{Tools: map[string]ToolEventRule{
		"create_audience":   {Event: "audience_created", Properties: func(map[string]any, any) map[string]any { panic("x") }},
		"list_destinations": {Event: "destinations_listed", When: func(map[string]any, any) bool { panic("x") }},
	}}})
	cs := connect(t, server, "2025-06-18", nil)
	callTool(t, cs, "create_audience", map[string]any{"name": "vip"}, nil)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if len(r.named("audience_created")) != 1 || len(r.named("destinations_listed")) != 0 {
		t.Errorf("events = %v", r.names())
	}
}

func TestCapabilityRequests(t *testing.T) {
	var called bool
	server := workspaceServer("workspace-mcp", nil)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if r, ok := req.(*mcp.CallToolRequest); ok && r.Params.Name == capabilityTool {
				called = true
			}
			return next(ctx, method, req)
		}
	})
	r := wrap(t, server, Options{CapabilityRequests: &CapabilityRequestOptions{Capabilities: []string{"bulk_export"}}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := tools.Tools[len(tools.Tools)-1]
	props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
	if tool.Name != capabilityTool || !slices.Equal(toStrings(props["capability"].(map[string]any)["enum"]), []string{"bulk_export", "other"}) {
		t.Fatalf("tool = %v", mustJSON(tool))
	}
	if _, ok := props["detail"]; ok {
		t.Error("detail offered without content capture")
	}
	res := callTool(t, cs, capabilityTool, map[string]any{"capability": "bulk_export", "detail": "export all"}, mcp.Meta{metaInteractionID: "int_9"})
	if res.IsError {
		t.Error("request_capability failed")
	}
	callTool(t, cs, capabilityTool, map[string]any{"capability": "teleport"}, nil)
	got := r.named("mcp_capability_requested")
	if len(got) != 2 || got[0].Properties["capability"] != "bulk_export" || got[0].Properties["interaction_id"] != "int_9" ||
		got[0].Properties["client_name"] != "test-client" || got[1].Properties["capability"] != "other" {
		t.Errorf("events = %v", mustJSON(got))
	}
	if called {
		t.Error("the server saw request_capability")
	}
	if len(r.named("mcp_tool_called")) != 0 || strings.Contains(mustJSON(r.all()), "export all") {
		t.Errorf("events = %v", r.names())
	}
}

func TestCapabilityDetailWithCapture(t *testing.T) {
	server := workspaceServer("workspace-mcp", nil)
	r := wrap(t, server, Options{CaptureContent: true, CapabilityRequests: &CapabilityRequestOptions{Capabilities: []string{"bulk_export"}}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, _ := cs.ListTools(context.Background(), nil)
	props := tools.Tools[len(tools.Tools)-1].InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["detail"]; !ok {
		t.Error("detail not offered with content capture")
	}
	for _, args := range []map[string]any{
		{"capability": "other", "detail": "merge two workspaces"},
		{"capability": "other", "detail": "email ann@example.com"},
		{"capability": "bulk_export", "detail": "ignored for known gaps"},
	} {
		callTool(t, cs, capabilityTool, args, nil)
	}
	var details []any
	for _, m := range r.named("mcp_capability_requested") {
		details = append(details, m.Properties["capability_detail"])
	}
	if want := []any{"merge two workspaces", "email [email]", nil}; !slices.Equal(details, want) {
		t.Errorf("details = %v", details)
	}
}

func TestNativeCapabilityToolNotShadowed(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "own-cap-mcp", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: capabilityTool, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "server own"}}}, nil
		})
	r := wrap(t, server, Options{CapabilityRequests: &CapabilityRequestOptions{Capabilities: []string{"x"}}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, _ := cs.ListTools(context.Background(), nil)
	if len(tools.Tools) != 1 {
		t.Errorf("tools = %v", mustJSON(tools))
	}
	res := callTool(t, cs, capabilityTool, map[string]any{}, nil)
	if res.Content[0].(*mcp.TextContent).Text != "server own" {
		t.Error("the server's own tool was shadowed")
	}
	if len(r.named("mcp_capability_requested")) != 0 || len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "res-mcp", Version: "1"}, nil)
	read := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "private body"}}}, nil
	}
	server.AddResource(&mcp.Resource{Name: "readme", URI: "docs://readme"}, read)
	server.AddResourceTemplate(&mcp.ResourceTemplate{Name: "destination", URITemplate: "destinations://{destination_id}"}, read)
	server.AddPrompt(&mcp.Prompt{Name: "audit_tracking_plan"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "private prompt"}}}}, nil
	})
	r := wrap(t, server, Options{})
	cs := connect(t, server, "2025-06-18", nil)
	ctx := context.Background()
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "docs://readme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListResources(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListResourceTemplates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"docs://readme", "destinations://dst_123"} {
		if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "nope://x"}); err == nil {
		t.Fatal("want a missing resource error")
	}
	if _, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "audit_tracking_plan"}); err != nil {
		t.Fatal(err)
	}
	var names []any
	for _, m := range r.named("mcp_resource_read") {
		names = append(names, m.Properties["resource_name"])
	}
	if want := []any{"unknown", "readme", "destinations://{destination_id}", "unknown"}; !slices.Equal(names, want) {
		t.Errorf("resource names = %v", names)
	}
	reads := r.named("mcp_resource_read")
	if reads[1].Properties["tool_status"] != "success" || reads[1].Properties["result_size_bytes"].(int) <= 0 || reads[3].Properties["tool_status"] != "error" {
		t.Errorf("reads = %v", mustJSON(reads))
	}
	p := r.props(t, "mcp_prompt_used")
	if p["prompt_name"] != "audit_tracking_plan" || p["tool_status"] != "success" {
		t.Errorf("prompt = %v", p)
	}
	all := mustJSON(r.all())
	if strings.Contains(all, "private") || strings.Contains(all, "dst_123") {
		t.Error("content or URI IDs leaked")
	}
}
