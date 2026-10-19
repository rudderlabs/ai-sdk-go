package ruddermcpgo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

const (
	metaInteractionID = "com.rudderstack/interaction_id"
	traceparent       = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
)

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// exercise runs what an agent does, against any transport and protocol version.
func exercise(t *testing.T, cs *sdk.ClientSession) {
	t.Helper()
	ctx := context.Background()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var capability *sdk.Tool
	for _, tool := range tools.Tools {
		props := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		if tool.Name == capabilityTool {
			capability = tool
			if !slices.Equal(toStrings(props["capability"].(map[string]any)["enum"]), []string{"bulk_export", "other"}) {
				t.Errorf("capability enum = %v", props["capability"])
			}
			if _, ok := props[requestContext]; ok {
				t.Error("request_context offered on request_capability")
			}
			continue
		}
		rc, ok := props[requestContext].(map[string]any)
		if !ok {
			t.Fatalf("%s: no request_context: %s", tool.Name, mustJSON(tool.InputSchema))
		}
		intent := rc["properties"].(map[string]any)["intent"].(map[string]any)
		if !slices.Equal(toStrings(intent["enum"]), []string{"debug_delivery", "build_audience", "other"}) {
			t.Errorf("intent enum = %v", intent["enum"])
		}
	}
	if capability == nil || capability.Annotations == nil || !capability.Annotations.ReadOnlyHint {
		t.Fatalf("request_capability not offered read-only: %s", mustJSON(capability))
	}
	meta := sdk.Meta{metaInteractionID: "int_1", "traceparent": traceparent, "other": "never copied"}
	callTool(t, cs, "list_destinations", map[string]any{
		"workspace_id": "ws_private_42", requestContext: map[string]any{"intent": "debug_delivery", "user_sentiment": "negative"},
	}, meta)
	callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "ws_private_42"}, meta)
	callTool(t, cs, "create_audience", map[string]any{"name": "vip", "extra": 1}, meta)
	if res := callTool(t, cs, capabilityTool, map[string]any{"capability": "teleport"}, meta); res.IsError {
		t.Errorf("request_capability failed: %s", mustJSON(res))
	}
	callTool(t, cs, capabilityTool, map[string]any{"capability": "bulk_export"}, meta)
	if res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fail", Arguments: map[string]any{}}); err == nil && !res.IsError {
		t.Error("fail succeeded")
	}
	if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "missing_tool", Arguments: map[string]any{}}); err == nil {
		t.Error("missing_tool succeeded")
	}
	if _, err := cs.ListResources(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListResourceTemplates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"docs://readme", "destinations://dst_123"} {
		if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "audit_tracking_plan"}); err != nil {
		t.Fatal(err)
	}
}

func fullOptions() Options {
	return Options{
		RequestContext:     rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
		CapabilityRequests: &rudderstackai.CapabilityRequestOptions{Capabilities: []string{"bulk_export"}},
		EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
			"create_audience": {Event: "audience_created", Properties: func(_ map[string]any, result any) map[string]any {
				out, _ := result.(map[string]any)
				return map[string]any{"audience_type": out["type"]}
			}},
		}},
	}
}

// check asserts the events of one exercise.
func check(t *testing.T, r *recorder, name, transport, protocol string, seen *handlerSeen) {
	t.Helper()
	if got := len(r.named("mcp_client_seen")); got != 1 {
		t.Fatalf("mcp_client_seen = %d: %v", got, r.names())
	}
	cs := r.props(t, "mcp_client_seen")
	for k, v := range map[string]any{
		"server_name": name, "server_version": "1.4.0", "client_name": "test-client", "client_version": "1.2.3",
		"transport": transport, "protocol_version": protocol, "auth_method": "none",
	} {
		if cs[k] != v {
			t.Errorf("mcp_client_seen %s = %v, want %v", k, cs[k], v)
		}
	}
	calls := r.named("mcp_tool_called")
	if len(calls) != 5 {
		t.Fatalf("mcp_tool_called = %d: %v", len(calls), r.names())
	}
	first := calls[0].Properties
	for k, v := range map[string]any{
		"tool_name": "list_destinations", "tool_status": "success", "is_error_result": false, "intent": "debug_delivery",
		"user_sentiment": "negative", "interaction_id": "int_1", "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
		"client_name": "test-client", "protocol_version": protocol, "call_index": 0, "retry_count": 0, "result_paginated": true,
	} {
		if first[k] != v {
			t.Errorf("first call %s = %v, want %v", k, first[k], v)
		}
	}
	if !slices.Equal(first["argument_keys"].([]string), []string{"workspace_id"}) {
		t.Errorf("argument_keys = %v", first["argument_keys"])
	}
	if first["request_id"] == "unknown" || first["request_id"] == "" {
		t.Errorf("request_id = %v", first["request_id"])
	}
	if _, ok := seen.last()[requestContext]; ok {
		t.Errorf("handler saw request_context: %v", seen.last())
	}
	second := calls[1].Properties
	if second["call_index"] != 1 || second["retry_count"] != 1 || second["ms_since_previous_call"] == nil {
		t.Errorf("second call = %v", second)
	}
	if p := r.find(t, "mcp_tool_called", "tool_name", "fail"); p["tool_status"] != "error" || p["error_type"] != "api_error" {
		t.Errorf("fail = %v", p)
	}
	if p := r.find(t, "mcp_tool_called", "tool_name", "missing_tool"); p["error_type"] != "invalid_request" {
		t.Errorf("missing_tool = %v", p)
	}
	if p := r.props(t, "audience_created"); p["audience_type"] != "computed" || p["tool_name"] != "create_audience" {
		t.Errorf("audience_created = %v", p)
	}
	caps := r.named("mcp_capability_requested")
	if len(caps) != 2 || caps[0].Properties["capability"] != "other" || caps[1].Properties["capability"] != "bulk_export" || caps[0].Properties["interaction_id"] != "int_1" {
		t.Errorf("mcp_capability_requested = %s", mustJSON(caps))
	}
	var names []any
	for _, m := range r.named("mcp_resource_read") {
		names = append(names, m.Properties["resource_name"])
	}
	if !slices.Equal(names, []any{"readme", "destinations://{destination_id}"}) {
		t.Errorf("resource names = %v", names)
	}
	if p := r.props(t, "mcp_prompt_used"); p["prompt_name"] != "audit_tracking_plan" || p["tool_status"] != "success" {
		t.Errorf("prompt = %v", p)
	}
	all := mustJSON(r.all())
	for _, leak := range []string{"ws_private_42", "secret", "private", "dst_123", "never copied", "teleport"} {
		if strings.Contains(all, leak) {
			t.Errorf("%q leaked into events", leak)
		}
	}
}

func TestStdio(t *testing.T) {
	seen := &handlerSeen{}
	name := "stdio-mcp"
	s := workspaceServer(name, seen)
	opts := fullOptions()
	opts.Transport = rudderstackai.TransportStdio
	r := wrap(t, s, opts)
	cs := connectStdio(t, s, "2025-06-18", nil)
	exercise(t, cs)
	check(t, r, name, "stdio", "2025-06-18", seen)
	if _, ok := r.props(t, "mcp_client_seen")["mcp_session_id"]; ok {
		t.Error("stdio has no session ID")
	}
}

func TestDeclaredToolEventOnValidationFailure(t *testing.T) {
	s := workspaceServer("workspace-mcp", nil)
	r := wrap(t, s, Options{EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	if res := callTool(t, cs, "create_audience", map[string]any{"type": "computed"}, nil); !res.IsError {
		t.Fatal("want a validation error result")
	}
	p := r.props(t, "audience_created")
	if p["tool_name"] != "create_audience" || p["tool_status"] != "error" {
		t.Errorf("declared event = %v", p)
	}
}

func TestStreamableHTTP(t *testing.T) {
	for _, protocol := range []string{"2025-06-18", ""} {
		t.Run("protocol "+protocol, func(t *testing.T) {
			seen := &handlerSeen{}
			name := "http-mcp-" + protocol
			s := workspaceServer(name, seen)
			r := wrap(t, s, fullOptions())
			cs := connectHTTP(t, s, protocol, nil)
			exercise(t, cs)
			negotiated := cs.InitializeResult().ProtocolVersion
			if protocol != "" && negotiated != protocol {
				t.Fatalf("negotiated %s", negotiated)
			}
			t.Logf("latest client negotiated %s", negotiated)
			check(t, r, name, "streamable_http", negotiated, seen)
			if cs.ID() != "" {
				if got := r.props(t, "mcp_client_seen")["mcp_session_id"]; got != cs.ID() {
					t.Errorf("mcp_session_id = %v, want %s", got, cs.ID())
				}
			}
		})
	}
}

func TestStateIsPerServerName(t *testing.T) {
	first, second := workspaceServer("shared-mcp", nil), workspaceServer("shared-mcp", nil)
	r1, r2 := wrap(t, first, Options{}), wrap(t, second, Options{})
	meta := sdk.Meta{metaInteractionID: "int_shared"}
	for _, s := range []*server.MCPServer{first, second} {
		callTool(t, connectStdio(t, s, "2025-06-18", nil), "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	}
	if got := len(r1.named("mcp_client_seen")) + len(r2.named("mcp_client_seen")); got != 1 {
		t.Errorf("mcp_client_seen = %d, want 1 for the name", got)
	}
	if p := r2.props(t, "mcp_tool_called"); p["call_index"] != 1 || p["retry_count"] != 1 {
		t.Errorf("counters not shared by name: %v", p)
	}
	other := workspaceServer("other-mcp", nil)
	r3 := wrap(t, other, Options{})
	callTool(t, connectStdio(t, other, "2025-06-18", nil), "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	if p := r3.props(t, "mcp_tool_called"); p["call_index"] != 0 {
		t.Errorf("counters shared across names: %v", p)
	}
}

func TestPerRequestServersKeepCounting(t *testing.T) {
	r := newRecorder(t)
	meta := sdk.Meta{metaInteractionID: "int_per_request"}
	for range 3 {
		s := workspaceServer("per-request-mcp", nil)
		if err := WrapServer(s, Options{Analytics: r}); err != nil {
			t.Fatal(err)
		}
		callTool(t, connectStdio(t, s, "2025-06-18", nil), "list_destinations", map[string]any{"workspace_id": "w"}, meta)
	}
	for i, c := range r.named("mcp_tool_called") {
		if c.Properties["call_index"] != i {
			t.Errorf("call %d = %v", i, c.Properties)
		}
	}
}

func TestWrappedServersAreReleased(t *testing.T) {
	r := newRecorder(t)
	const n = 100
	for i := range n {
		if err := WrapServer(workspaceServer("released-"+strconv.Itoa(i%3), nil), Options{Analytics: r}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	size := func() int {
		instances.Lock()
		defer instances.Unlock()
		return len(instances.m)
	}
	for size() >= n/2 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := size(); got >= n/2 {
		t.Errorf("%d of %d wrapped servers still held", got, n)
	}
}

func TestIdentifyFromContext(t *testing.T) {
	type userKey struct{}
	s := workspaceServer("identify-mcp", nil)
	r := wrap(t, s, Options{Identify: func(ctx context.Context) (rudderstackai.Identity, error) {
		if ctx.Value(userKey{}) != nil {
			return rudderstackai.Identity{}, errors.New("unexpected")
		}
		return rudderstackai.Identity{UserID: "user_1", AccountID: "ws_1", AuthMethod: "api_key"}, nil
	}})
	callTool(t, connectStdio(t, s, "2025-06-18", nil), "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	call := r.named("mcp_tool_called")[0]
	if call.UserId != "user_1" || call.Properties["account_id"] != "ws_1" || r.props(t, "mcp_client_seen")["auth_method"] != "api_key" {
		t.Errorf("identity = %s / %v", call.UserId, call.Properties)
	}
}

func TestOptionsValidation(t *testing.T) {
	s := workspaceServer("validation-mcp", nil)
	if err := WrapServer(s, Options{}); err == nil {
		t.Error("missing client: want an error")
	}
	if err := WrapServer(nil, Options{Analytics: newRecorder(t)}); err == nil {
		t.Error("nil server: want an error")
	}
	r := wrap(t, s, Options{})
	if err := WrapServer(s, Options{Analytics: r, Transport: "pigeon"}); err == nil {
		t.Error("rewrap with a bad transport: want an error")
	}
}

func TestRewrapReplacesOptionsAndCapabilityTool(t *testing.T) {
	s := workspaceServer("rewrap-mcp", nil)
	wrap(t, s, Options{CapabilityRequests: &rudderstackai.CapabilityRequestOptions{}})
	if s.GetTool(capabilityTool) == nil {
		t.Fatal("request_capability not registered")
	}
	r := wrap(t, s, Options{})
	if s.GetTool(capabilityTool) != nil {
		t.Error("request_capability kept after rewrapping without it")
	}
	callTool(t, connectStdio(t, s, "2025-06-18", nil), "list_destinations", map[string]any{"workspace_id": "w"}, nil)
	if len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestNativeCapabilityToolNotShadowed(t *testing.T) {
	s := server.NewMCPServer("own-cap-mcp", "1", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool(capabilityTool), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("server own"), nil
	})
	r := wrap(t, s, Options{CapabilityRequests: &rudderstackai.CapabilityRequestOptions{}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Description != "" {
		t.Errorf("tools = %s", mustJSON(tools))
	}
	res := callTool(t, cs, capabilityTool, map[string]any{}, nil)
	if res.Content[0].(*sdk.TextContent).Text != "server own" {
		t.Error("the server's own tool was shadowed")
	}
	if len(r.named("mcp_capability_requested")) != 0 || len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestRequestElicitation(t *testing.T) {
	s := server.NewMCPServer("elicit-mcp", "1", server.WithToolCapabilities(true), server.WithElicitation())
	s.AddTool(mcp.NewTool("ask"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := RequestElicitation(ctx, s, mcp.ElicitationRequest{Params: mcp.ElicitationParams{
			Message: "Your secret answer?", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}})
		if err != nil {
			return mcp.NewToolResultError("no answer"), nil
		}
		return mcp.NewToolResultText(string(res.Action)), nil
	})
	r := wrap(t, s, Options{})
	cs := connectStdio(t, s, "2025-06-18", &sdk.ClientOptions{ElicitationHandler: func(context.Context, *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
		return &sdk.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
	}})
	callTool(t, cs, "ask", map[string]any{}, sdk.Meta{metaInteractionID: "int_e"})
	p := r.props(t, "mcp_elicitation_completed")
	call := r.props(t, "mcp_tool_called")
	if p["elicitation_action"] != "accept" || p["tool_name"] != "ask" || p["request_id"] != call["request_id"] || p["interaction_id"] != "int_e" || p["duration_ms"] == nil {
		t.Errorf("elicitation = %v", p)
	}
	if strings.Contains(mustJSON(r.all()), "secret") {
		t.Error("question leaked")
	}
}

func TestMCPEventsHelpers(t *testing.T) {
	s := workspaceServer("events-mcp", nil)
	r := wrap(t, s, Options{MCPEvents: true})
	ctx := context.Background()
	TrackMCPEventSubscription(ctx, s, rudderstackai.SubscriptionChange{
		Action: rudderstackai.SubscriptionCreated, SubscriptionID: "sub_1", EventName: "comment.created",
		EventsIdentity: rudderstackai.EventsIdentity{UserID: "u1"},
	})
	send := WrapMCPEventSender(s, func(context.Context, rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
		return rudderstackai.DeliveryOutcome{HTTPStatus: 200, AttemptCount: 1}, nil
	})
	if _, err := send(ctx, rudderstackai.EventDelivery{SubscriptionID: "sub_1", EventName: "comment.created", EventID: "evt_1"}); err != nil {
		t.Fatal(err)
	}
	sub, del := r.props(t, "mcp_event_subscription_changed"), r.props(t, "mcp_event_delivered")
	if sub["server_name"] != "events-mcp" || del["delivery_status"] != "acknowledged" || del["delivery_latency_ms"] != nil {
		t.Errorf("events = %v / %v", sub, del)
	}
	unwrapped := server.NewMCPServer("unwrapped", "1")
	TrackMCPEventSubscription(ctx, unwrapped, rudderstackai.SubscriptionChange{Action: "created", SubscriptionID: "s", EventName: "e"})
	if len(r.all()) != 2 {
		t.Errorf("events = %v", r.names())
	}
}

// A record without its required IDs is dropped with a warning, as on the go-sdk.
func TestMCPEventsMissingIDsDropped(t *testing.T) {
	s := workspaceServer("events-ids-mcp", nil)
	r := wrap(t, s, Options{MCPEvents: true})
	hook.Reset() // fresh once-per-process warnings
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := context.Background()
	TrackMCPEventSubscription(ctx, s, rudderstackai.SubscriptionChange{Action: rudderstackai.SubscriptionCreated, EventName: "e"})
	send := WrapMCPEventSender(s, func(context.Context, rudderstackai.EventDelivery) (rudderstackai.DeliveryOutcome, error) {
		return rudderstackai.DeliveryOutcome{HTTPStatus: 200}, nil
	})
	if _, err := send(ctx, rudderstackai.EventDelivery{SubscriptionID: "s", EventName: "e"}); err != nil {
		t.Fatal(err)
	}
	if len(r.all()) != 0 {
		t.Errorf("events = %v", r.names())
	}
	for _, want := range []string{"mcp_event_subscription_changed dropped", "mcp_event_delivered dropped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no warning %q in %s", want, logs.String())
		}
	}
}

func TestErrorTypeMapping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{context.Background(), errors.New("x"), "api_error"},
		{ctx, errors.New("x"), "cancelled"},
		{context.Background(), context.DeadlineExceeded, "timeout"},
		{context.Background(), codeError(mcp.INVALID_PARAMS), "invalid_request"},
	}
	for _, c := range cases {
		if got := errorType(c.ctx, c.err); got != c.want {
			t.Errorf("errorType(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
