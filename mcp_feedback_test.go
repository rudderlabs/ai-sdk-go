package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestFeedbackRequests(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, protocol := range []string{"2025-06-18", "2026-07-28"} {
			for _, transport := range []string{"stdio", "http", "stateless"} {
				t.Run(protocol+"/"+transport+map[bool]string{false: "/private", true: "/capture"}[capture], func(t *testing.T) {
					s := workspaceServer("feedback-mcp", nil)
					r := wrap(t, s, Options{CaptureContent: capture, RequestContext: RequestContextOptions{Enabled: true}, FeedbackRequests: &FeedbackRequestOptions{Capabilities: []string{"export", "export", "other"}}})
					var cs *mcp.ClientSession
					if transport == "stdio" {
						cs = connect(t, s, protocol, nil)
					} else {
						cs = connectHTTP(t, httpServer(t, s, &mcp.StreamableHTTPOptions{Stateless: transport == "stateless"}), protocol)
					}
					tools, err := cs.ListTools(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					var tool *mcp.Tool
					for _, candidate := range tools.Tools {
						if candidate.Name == feedbackTool {
							tool = candidate
						}
					}
					if tool == nil {
						t.Fatal("submit_feedback missing")
					}
					listed := map[string]any{}
					if err := json.Unmarshal([]byte(mustJSON(tool)), &listed); err != nil {
						t.Fatal(err)
					}
					annotations := listed["annotations"].(map[string]any)
					if listed["title"] != "Send feedback" || annotations["title"] != "Send feedback" || annotations["readOnlyHint"] != false || annotations["destructiveHint"] != false || annotations["idempotentHint"] != false || annotations["openWorldHint"] != capture {
						t.Fatalf("metadata = %s", mustJSON(tool))
					}
					schema := listed["inputSchema"].(map[string]any)
					props := schema["properties"].(map[string]any)
					if schema["additionalProperties"] != false || props[requestContext] != nil || !slices.Equal(toStrings(props["capability"].(map[string]any)["enum"]), []string{"export", "other"}) {
						t.Fatalf("schema = %s", mustJSON(schema))
					}
					required := []string{"category"}
					if capture {
						required = append(required, "feedback")
					}
					if !slices.Equal(toStrings(schema["required"]), required) {
						t.Fatalf("required = %v", schema["required"])
					}
					if _, present := props["feedback"]; present != capture {
						t.Fatal("feedback capture schema mismatch")
					}
					if capture && props["feedback"].(map[string]any)["maxLength"] != float64(2000) {
						t.Fatal("feedback limit")
					}
					if strings.Contains(listed["description"].(string), "do not paste customer data") != capture {
						t.Fatal("capture description mismatch")
					}
					valid := map[string]any{"category": "feature_request", "tool_name": "list_destinations", "capability": "export"}
					if capture {
						valid["feedback"] = "email ann@example.com"
					}
					refused := []map[string]any{
						{"category": "invalid"}, {"category": false}, {"category": nil}, {"category": []string{"other"}}, {"category": map[string]any{}}, {},
						{"tool_name": "unlisted"}, {"tool_name": ""}, {"tool_name": 7}, {"capability": true}, {"category": "bug_report", "capability": "export"}, {"extra": "value"},
					}
					if capture {
						refused = append(refused, map[string]any{"feedback": ""}, map[string]any{"feedback": " \t\n"}, map[string]any{"feedback": "\u200b\x00"}, map[string]any{"feedback": strings.Repeat("é", 2001)}, map[string]any{"feedback": nil}, map[string]any{"feedback": 7})
					} else {
						refused = append(refused, map[string]any{"feedback": "private"})
					}
					for _, overrides := range refused {
						args := maps.Clone(valid)
						for key, value := range overrides {
							args[key] = value
						}
						if len(overrides) == 0 {
							delete(args, "category")
						}
						before := len(r.all())
						res := callTool(t, cs, feedbackTool, args, nil)
						if !res.IsError || len(res.Content) == 0 || res.Content[0].(*mcp.TextContent).Text == "" {
							t.Errorf("not refused: %s => %s", mustJSON(args), mustJSON(res))
						}
						if len(r.all()) != before {
							t.Errorf("refusal emitted event: %s", mustJSON(args))
						}
					}
					if capture {
						args := maps.Clone(valid)
						delete(args, "feedback")
						before := len(r.all())
						if res := callTool(t, cs, feedbackTool, args, nil); !res.IsError || len(r.all()) != before {
							t.Fatal("missing feedback accepted or emitted an event")
						}
					}
					res := callTool(t, cs, feedbackTool, valid, mcp.Meta{metaInteractionID: "int_feedback"})
					if res.IsError || res.Content[0].(*mcp.TextContent).Text != "Sent to the server owner. Tell the user it was sent; nobody replies." {
						t.Fatalf("reply = %s", mustJSON(res))
					}
					events := r.named("mcp_feedback_submitted")
					p := events[0].Properties
					if p["feedback_category"] != "feature_request" || p["feedback_tool_name"] != "list_destinations" || p["capability"] != "export" || p["interaction_id"] != "int_feedback" || p["client_name"] != "test-client" {
						t.Fatalf("event = %s", mustJSON(p))
					}
					if capture {
						if p["feedback_text"] != "email [email]" {
							t.Fatalf("masked text = %v", p["feedback_text"])
						}
					} else if _, ok := p["feedback_text"]; ok {
						t.Fatal("text captured")
					}
					valid["capability"] = "unlisted"
					valid["tool_name"] = feedbackTool
					callTool(t, cs, feedbackTool, valid, nil)
					if r.named("mcp_feedback_submitted")[1].Properties["capability"] != "other" {
						t.Fatal("unknown capability did not map to other")
					}
					delete(valid, "capability")
					delete(valid, "tool_name")
					callTool(t, cs, feedbackTool, valid, nil)
					for _, category := range []string{"bug_report", "documentation_issue", "access_issue", "performance_issue", "positive_feedback", "other"} {
						valid["category"] = category
						callTool(t, cs, feedbackTool, valid, nil)
					}
					for _, event := range r.named("mcp_feedback_submitted")[2:] {
						if event.Properties["capability"] != nil || event.Properties["feedback_tool_name"] != nil {
							t.Fatal("optional fields present")
						}
					}
					if len(r.named("mcp_tool_called")) != 0 || len(r.named("mcp_capability_requested")) != 0 {
						t.Fatal("extra tool events")
					}
				})
			}
		}
	}
}

func TestFeedbackMasking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		redact  func(string) (string, error)
		disable bool
		text    string
		want    any
	}{
		{name: "failure", redact: func(string) (string, error) { return "", errors.New("failed") }, text: "problem"},
		{name: "panic", redact: func(string) (string, error) { panic("failed") }, text: "problem"},
		{name: "empty", redact: func(string) (string, error) { return "", nil }, text: "problem"},
		{name: "sensitive", redact: func(string) (string, error) { return "ann@example.com", nil }, text: "problem"},
		{name: "cap", redact: func(string) (string, error) { return strings.Repeat("é", 2001), nil }, text: "problem", want: strings.Repeat("é", 2000)},
		{name: "unicode", text: strings.Repeat("é", 2000), want: strings.Repeat("é", 2000)},
		{name: "disabled", disable: true, text: "ann@example.com", want: "ann@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer("feedback-mask-mcp", nil)
			r := wrap(t, s, Options{CaptureContent: true, FeedbackRequests: &FeedbackRequestOptions{}, Redact: tc.redact, DisableRedaction: tc.disable})
			cs := connect(t, s, "2025-06-18", nil)
			res := callTool(t, cs, feedbackTool, map[string]any{"category": "other", "feedback": tc.text}, nil)
			events := r.named("mcp_feedback_submitted")
			if res.IsError || len(events) != 1 || events[0].Properties["feedback_text"] != tc.want {
				t.Fatalf("result=%s events=%s", mustJSON(res), mustJSON(events))
			}
		})
	}
}

func TestFeedbackBothOptionsAndRewrap(t *testing.T) {
	s := workspaceServer("feedback-options-mcp", nil)
	r := newRecorder(t)
	invalid := Options{Analytics: r, CapabilityRequests: &CapabilityRequestOptions{}, FeedbackRequests: &FeedbackRequestOptions{}}
	if err := WrapMCPServer(s, invalid); err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("error=%v", err)
	}
	if err := WrapMCPServer(s, Options{Analytics: r, FeedbackRequests: &FeedbackRequestOptions{}}); err != nil {
		t.Fatal(err)
	}
	if err := WrapMCPServer(s, invalid); err == nil {
		t.Fatal("invalid rewrap accepted")
	}
	cs := connect(t, s, "2025-06-18", nil)
	tools, _ := cs.ListTools(context.Background(), nil)
	if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == feedbackTool }) {
		t.Fatal("invalid rewrap changed options")
	}
	if err := WrapMCPServer(s, Options{Analytics: r}); err != nil {
		t.Fatal(err)
	}
	tools, _ = cs.ListTools(context.Background(), nil)
	if slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == feedbackTool }) {
		t.Fatal("feedback not removed")
	}
}

func TestNativeFeedbackToolNotShadowed(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "own-cap-mcp", Version: "1"}, nil)
	openWorld, destructive := true, true
	native := &mcp.Tool{Name: feedbackTool, Title: "Server capability", Description: "Server tool",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{Title: "Server annotation", OpenWorldHint: &openWorld, DestructiveHint: &destructive}}
	server.AddTool(native,
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "server own"}}}, nil
		})
	want := mustJSON(native)
	r := wrap(t, server, Options{FeedbackRequests: &FeedbackRequestOptions{Capabilities: []string{"x"}}})
	cs := connect(t, server, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || mustJSON(tools.Tools[0]) != want {
		t.Errorf("tools = %v", mustJSON(tools))
	}
	if mustJSON(native) != want {
		t.Error("registered server tool changed")
	}
	res := callTool(t, cs, feedbackTool, map[string]any{}, nil)
	if res.Content[0].(*mcp.TextContent).Text != "server own" {
		t.Error("the server's own tool was shadowed")
	}
	if len(r.named("mcp_feedback_submitted")) != 0 || len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestFeedbackServerVersionEveryEvent(t *testing.T) {
	for _, version := range []string{"1.2.3", ""} {
		t.Run(version, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: "version-test", Version: version}, nil)
			r := wrap(t, s, Options{MCPEvents: true, FeedbackRequests: &FeedbackRequestOptions{Capabilities: []string{"export"}}})
			server, _ := instances.get(s)
			c := server.Begin(mcpbridge.Request{Ctx: context.Background(), InitializeClient: &mcpbridge.Client{Name: "client"}})
			c.Initialized(nil, nil, "2025-06-18")
			c.ToolCall("tool", nil, false, nil, "")
			c.ToolDone(mcpbridge.Outcome{Result: &mcp.CallToolResult{}})
			c.Finished("resources/read", "resource", 0, "")
			c.Finished("prompts/get", "prompt", 0, "")
			c.Feedback(map[string]json.RawMessage{"category": json.RawMessage(`"other"`)})
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

func TestFeedbackRequestOAuthEveryEvent(t *testing.T) {
	for _, id := range []Identity{{OAuthClientID: "app", AuthMethod: "api_key", TokenScopes: []string{"private"}, Country: "IN"}, {}} {
		s := workspaceServer(t.Name(), nil)
		r := wrap(t, s, Options{Identify: func(context.Context, *auth.TokenInfo) (Identity, error) { return id, nil }, MCPEvents: true,
			FeedbackRequests: &FeedbackRequestOptions{Capabilities: []string{"export"}},
			EventRules:       EventRules{Tools: map[string]ToolEventRule{"tool": {Event: "performed"}}}})
		w, _ := instances.get(s)
		c := w.Begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: []mcpbridge.Tool{{Name: "tool"}}})
		c.ToolCall("tool", nil, false, nil, "")
		c.ToolDone(mcpbridge.Outcome{Result: &mcp.CallToolResult{}})
		c.Finished("resources/read", "resource", 0, "")
		c.Finished("prompts/get", "prompt", 0, "")
		c.Feedback(map[string]json.RawMessage{"category": json.RawMessage(`"other"`)})
		w.Elicited(c, mcpbridge.Elicitation{Action: "accept"})
		TrackMCPEventSubscription(context.Background(), s, SubscriptionChange{Action: SubscriptionCreated, SubscriptionID: "sub", EventName: "update"})
		send := WrapMCPEventSender(s, func(context.Context, EventDelivery) (DeliveryOutcome, error) {
			return DeliveryOutcome{HTTPStatus: 200}, nil
		})
		if _, err := send(context.Background(), EventDelivery{SubscriptionID: "sub", EventID: "event", EventName: "update"}); err != nil {
			t.Fatal(err)
		}
		if len(r.all()) != 8 {
			t.Fatalf("events = %v", r.names())
		}
		for _, event := range r.all() {
			p := event.Properties
			if event.Event == "mcp_event_subscription_changed" || event.Event == "mcp_event_delivered" {
				for _, key := range []string{"oauth_client_id", "auth_method"} {
					if _, ok := p[key]; ok {
						t.Errorf("%s has %s", event.Event, key)
					}
				}
				continue
			}
			method := id.AuthMethod
			if method == "" {
				method = "none"
			}
			if str(p["oauth_client_id"]) != id.OAuthClientID || p["auth_method"] != method {
				t.Errorf("%s auth = %v", event.Event, p)
			}
			for _, key := range []string{"token_scopes", "country"} {
				if _, ok := p[key]; ok {
					t.Errorf("%s has %s", event.Event, key)
				}
			}
		}
	}
}

func TestFeedbackAnalyticsFailuresNeverReachTheServer(t *testing.T) {
	for _, client := range []broken{{panics: true}, {}} {
		server := workspaceServer("workspace-mcp", nil)
		freshState(t)
		if err := WrapMCPServer(server, Options{Analytics: client, FeedbackRequests: &FeedbackRequestOptions{}}); err != nil {
			t.Fatal(err)
		}
		cs := connect(t, server, "2025-06-18", nil)
		if res := callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w"}, nil); res.IsError {
			t.Fatal("the tool call failed")
		}
		if res := callTool(t, cs, feedbackTool, map[string]any{"category": "other"}, nil); res.IsError {
			t.Fatal("request_capability failed")
		}
	}
}

func TestFeedbackUsesCurrentRegistry(t *testing.T) {
	s := workspaceServer("feedback-registry", nil)
	r := wrap(t, s, Options{FeedbackRequests: &FeedbackRequestOptions{}})
	cs := connect(t, s, "2025-06-18", nil)
	s.AddTool(&mcp.Tool{Name: "new_tool", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	args := map[string]any{"category": "other", "tool_name": "new_tool"}
	if res := callTool(t, cs, feedbackTool, args, nil); res.IsError {
		t.Fatal("newly registered tool refused")
	}
	s.RemoveTools("new_tool")
	before := len(r.all())
	if res := callTool(t, cs, feedbackTool, args, nil); !res.IsError || len(r.all()) != before {
		t.Fatal("removed tool accepted or emitted an event")
	}
	native := &mcp.Tool{Name: feedbackTool, Description: "server own", InputSchema: map[string]any{"type": "object"}}
	s.AddTool(native, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "native"}}}, nil
	})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == feedbackTool && mustJSON(tool) == mustJSON(native) }) {
		t.Fatal("native override changed")
	}
	if res := callTool(t, cs, feedbackTool, map[string]any{}, nil); res.IsError || res.Content[0].(*mcp.TextContent).Text != "native" {
		t.Fatal("native override intercepted")
	}
	if len(r.named("mcp_feedback_submitted")) != 1 || len(r.named("mcp_tool_called")) != 1 {
		t.Fatalf("events=%v", r.names())
	}
}
