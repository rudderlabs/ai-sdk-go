package ruddermcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestFeedbackRequests(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, protocol := range []string{"2025-06-18", "2026-07-28"} {
			for _, transport := range []string{"stdio", "http", "stateless"} {
				t.Run(protocol+"/"+transport+map[bool]string{false: "/private", true: "/capture"}[capture], func(t *testing.T) {
					s := workspaceServer("feedback-mcp", nil)
					r := wrap(t, s, Options{CaptureContent: capture, RequestContext: rudderstackai.RequestContextOptions{Enabled: true}, FeedbackRequests: &rudderstackai.FeedbackRequestOptions{Capabilities: []string{"export", "export", "other"}}})
					var cs *sdk.ClientSession
					switch transport {
					case "stdio":
						cs = connectStdio(t, s, protocol, nil)
					case "stateless":
						cs = connectHTTP(t, s, protocol, nil, server.WithStateLess(true))
					default:
						cs = connectHTTP(t, s, protocol, nil)
					}
					tools, err := cs.ListTools(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					var tool *sdk.Tool
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
						if !res.IsError || len(res.Content) == 0 || res.Content[0].(*sdk.TextContent).Text == "" {
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
					res := callTool(t, cs, feedbackTool, valid, sdk.Meta{metaInteractionID: "int_feedback"})
					if res.IsError || res.Content[0].(*sdk.TextContent).Text != "Sent to the server owner. Tell the user it was sent; nobody replies." {
						t.Fatalf("reply = %s", mustJSON(res))
					}
					events := r.named("mcp_feedback_submitted")
					p := events[0].Properties
					if p["feedback_category"] != "feature_request" || p["feedback_tool_name"] != "list_destinations" || p["capability"] != "export" || p["interaction_id"] != "int_feedback" {
						t.Fatalf("event = %s", mustJSON(p))
					}
					// Stateless 2025 requests carry no client; older mcp-go minimums cannot negotiate 2026-07-28.
					if (transport != "stateless" || p["protocol_version"] == "2026-07-28") && p["client_name"] != "test-client" {
						t.Fatalf("client identity = %v", p)
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
			r := wrap(t, s, Options{CaptureContent: true, FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}, Redact: tc.redact, DisableRedaction: tc.disable})
			cs := connectStdio(t, s, "2025-06-18", nil)
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
	invalid := Options{Analytics: r, CapabilityRequests: &rudderstackai.CapabilityRequestOptions{}, FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}}
	if err := WrapServer(s, invalid); err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("error=%v", err)
	}
	if err := WrapServer(s, Options{Analytics: r, FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}}); err != nil {
		t.Fatal(err)
	}
	if err := WrapServer(s, invalid); err == nil {
		t.Fatal("invalid rewrap accepted")
	}
	cs := connectStdio(t, s, "2025-06-18", nil)
	tools, _ := cs.ListTools(context.Background(), nil)
	if !slices.ContainsFunc(tools.Tools, func(tool *sdk.Tool) bool { return tool.Name == feedbackTool }) {
		t.Fatal("invalid rewrap changed options")
	}
	if err := WrapServer(s, Options{Analytics: r}); err != nil {
		t.Fatal(err)
	}
	tools, _ = cs.ListTools(context.Background(), nil)
	if slices.ContainsFunc(tools.Tools, func(tool *sdk.Tool) bool { return tool.Name == feedbackTool }) {
		t.Fatal("feedback not removed")
	}
}

func TestNativeFeedbackToolNotShadowed(t *testing.T) {
	s := server.NewMCPServer("own-cap-mcp", "1", server.WithToolCapabilities(true))
	native := mcp.NewTool(feedbackTool, mcp.WithToolTitle("Server capability"), mcp.WithDescription("Server tool"),
		mcp.WithTitleAnnotation("Server annotation"), mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true), mcp.WithIdempotentHintAnnotation(false), mcp.WithOpenWorldHintAnnotation(true))
	s.AddTool(native, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("server own"), nil
	})
	want := mustJSON(s.GetTool(feedbackTool).Tool)
	var expected sdk.Tool
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	r := wrap(t, s, Options{FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Title != expected.Title || tools.Tools[0].Description != expected.Description ||
		mustJSON(tools.Tools[0].Annotations) != mustJSON(expected.Annotations) {
		t.Errorf("tools = %s", mustJSON(tools))
	}
	if mustJSON(s.GetTool(feedbackTool).Tool) != want {
		t.Error("registered server tool changed")
	}
	res := callTool(t, cs, feedbackTool, map[string]any{}, nil)
	if res.Content[0].(*sdk.TextContent).Text != "server own" {
		t.Error("the server's own tool was shadowed")
	}
	if len(r.named("mcp_feedback_submitted")) != 0 || len(r.named("mcp_tool_called")) != 1 {
		t.Errorf("events = %v", r.names())
	}
}

func TestFeedbackUsesCurrentRegistry(t *testing.T) {
	s := workspaceServer("feedback-registry", nil)
	r := wrap(t, s, Options{FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}})
	cs := connectStdio(t, s, "2025-06-18", nil)
	s.AddTool(mcp.NewTool("new_tool"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	args := map[string]any{"category": "other", "tool_name": "new_tool"}
	if res := callTool(t, cs, feedbackTool, args, nil); res.IsError {
		t.Fatal("newly registered tool refused")
	}
	s.DeleteTools("new_tool")
	before := len(r.all())
	if res := callTool(t, cs, feedbackTool, args, nil); !res.IsError || len(r.all()) != before {
		t.Fatal("removed tool accepted or emitted an event")
	}
	native := mcp.NewTool(feedbackTool, mcp.WithDescription("server own"))
	s.AddTool(native, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("native"), nil
	})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools.Tools, func(tool *sdk.Tool) bool { return tool.Name == feedbackTool && tool.Description == native.Description }) {
		t.Fatal("native override changed")
	}
	if res := callTool(t, cs, feedbackTool, map[string]any{}, nil); res.IsError || res.Content[0].(*sdk.TextContent).Text != "native" {
		t.Fatal("native override intercepted")
	}
	if len(r.named("mcp_feedback_submitted")) != 1 || len(r.named("mcp_tool_called")) != 1 {
		t.Fatalf("events=%v", r.names())
	}
}
