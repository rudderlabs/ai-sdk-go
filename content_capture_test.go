package rudderstackai

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
)

// captureAll is the policy of the bool option.
func captureAll(on bool) capture.Resolved { return capture.Resolve(capture.Bool(on)) }

// globalCapture sets the global capture for one test.
func globalCapture(t *testing.T, opts ...ConfigOption) {
	t.Helper()
	resetForTest()
	if err := Configure(opts...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resetForTest)
}

func offered(t *testing.T, cs *mcp.ClientSession, key string) bool {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schema := tools.Tools[0].InputSchema.(map[string]any)
	rc := schema["properties"].(map[string]any)[requestContext].(map[string]any)
	_, ok := rc["properties"].(map[string]any)[key]
	return ok
}

// SDK-6888: an unset MCP CaptureContent follows the global setting, and the policy decides per kind.
func TestMCPContentCaptureFollowsGlobalAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name               string
		global             []ConfigOption
		opts               Options
		reason, capability bool
	}{
		{"unset inherits global on", []ConfigOption{WithCaptureContent(true)}, Options{}, true, true},
		{"unset inherits global off", nil, Options{}, false, false},
		{"explicit off over global on", []ConfigOption{WithCaptureContent(true)}, Options{ContentCapture: &ContentCapture{Default: Ptr(false)}}, false, false},
		{"call reason only", nil, Options{ContentCapture: &ContentCapture{CallReason: Ptr(true)}}, true, false},
		{"global policy per kind", []ConfigOption{WithContentCapture(ContentCapture{FeedbackText: Ptr(true)})}, Options{}, false, true},
		{"server key over global default", []ConfigOption{WithCaptureContent(true)}, Options{ContentCapture: &ContentCapture{CallReason: Ptr(false)}}, false, true},
		{"bool on over global policy off", []ConfigOption{WithContentCapture(ContentCapture{Default: Ptr(false)})}, Options{CaptureContent: true}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalCapture(t, tc.global...)
			server := workspaceServer("workspace-mcp", nil)
			tc.opts.RequestContext = RequestContextOptions{Enabled: true}
			tc.opts.CapabilityRequests = &CapabilityRequestOptions{Capabilities: []string{"bulk_export"}}
			r := wrap(t, server, tc.opts)
			cs := connect(t, server, "2025-06-18", nil)
			if got := offered(t, cs, "call_reason"); got != tc.reason {
				t.Errorf("call_reason offered = %v, want %v", got, tc.reason)
			}
			callTool(t, cs, "list_destinations", map[string]any{"workspace_id": "w", requestContext: map[string]any{"call_reason": "check delivery"}}, nil)
			if _, got := r.props(t, "mcp_tool_called")["call_reason"]; got != tc.reason {
				t.Errorf("call_reason sent = %v, want %v", got, tc.reason)
			}
			callTool(t, cs, capabilityTool, map[string]any{"capability": "other", "detail": "merge two workspaces"}, nil)
			if _, got := r.props(t, "mcp_capability_requested")["capability_detail"]; got != tc.capability {
				t.Errorf("capability_detail sent = %v, want %v", got, tc.capability)
			}
		})
	}
}

// SDK-6888 D2: only an explicit IntentSummary key withholds intent_summary; the bool form does not.
func TestIntentSummaryOnlyExplicitKeyGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ConfigOption
		sent bool
	}{
		{"capture off (as before)", nil, true},
		{"default off", []ConfigOption{WithContentCapture(ContentCapture{Default: Ptr(false)})}, true},
		{"explicit off", []ConfigOption{WithCaptureContent(true), WithContentCapture(ContentCapture{IntentSummary: Ptr(false)})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t, tc.opts...)
			TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "Refund question."})
			if _, got := r.props(t, "ai_interaction_classified")["intent_summary"]; got != tc.sent {
				t.Fatalf("intent_summary sent = %v, want %v", got, tc.sent)
			}
		})
	}
}

// Per-kind flags: a score explanation follows ScoreExplanation, not the other kinds.
func TestScoreExplanationFollowsItsFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ContentCapture
		sent   bool
	}{
		{"explanation only", ContentCapture{ScoreExplanation: Ptr(true)}, true},
		{"everything but explanation", ContentCapture{Default: Ptr(true), ScoreExplanation: Ptr(false)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t, WithContentCapture(tc.policy))
			TrackScore(context.Background(), TrackScoreOptions{Score: Score{EvaluationName: "helpful", Evaluator: EvaluatorHuman, ScoreValue: Ptr(1.0), Explanation: "Clear answer."}, InteractionID: "i", ConversationID: "c"})
			if _, got := r.props(t, "ai_interaction_scored")["explanation"]; got != tc.sent {
				t.Fatalf("explanation sent = %v, want %v", got, tc.sent)
			}
		})
	}
}

// Per-call content follows LLMCalls: answer text alone does not send it, LLMCalls alone does.
func TestCallContentFollowsLLMCalls(t *testing.T) {
	for _, tc := range []struct {
		name          string
		policy        ContentCapture
		call, answers bool
	}{
		{"llm calls only", ContentCapture{LLMCalls: Ptr(true)}, true, false},
		{"answer text only", ContentCapture{AnswerText: Ptr(true)}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithContentCapture(tc.policy))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", PromptText: "hello"})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "hi", InputMessages: []GenAIMessage{textMessage("user", "hello")}})
			i.End(EndOptions{ResponseText: "hi"})
			p := callEvents(t, r)[0]
			if _, got := p["input_messages"]; got != tc.call {
				t.Errorf("input_messages sent = %v, want %v", got, tc.call)
			}
			if _, got := p["output_text"]; got != tc.call {
				t.Errorf("output_text sent = %v, want %v", got, tc.call)
			}
			if _, got := r.props(t, "ai_user_prompt_created")["prompt_text"]; got != tc.answers {
				t.Errorf("prompt_text sent = %v, want %v", got, tc.answers)
			}
		})
	}
}

// The injected submit_feedback schema and request_context follow the live policy after wrapping.
func TestInjectedSchemasFollowLiveGlobalPolicy(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "off then on", true: "on then off"}[before], func(t *testing.T) {
			globalCapture(t, WithCaptureContent(before))
			server := workspaceServer("feedback-live-mcp", nil)
			r := wrap(t, server, Options{RequestContext: RequestContextOptions{Enabled: true}, FeedbackRequests: &FeedbackRequestOptions{}})
			cs := connect(t, server, "2025-06-18", nil)
			if err := Configure(WithCaptureContent(!before)); err != nil {
				t.Fatal(err)
			}
			if got := offered(t, cs, "call_reason"); got != !before {
				t.Fatalf("call_reason offered = %v", got)
			}
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			for _, tool := range tools.Tools {
				if tool.Name == feedbackTool {
					schema = tool.InputSchema.(map[string]any)
				}
			}
			_, has := schema["properties"].(map[string]any)["feedback"]
			if has != !before {
				t.Fatalf("feedback offered = %v", has)
			}
			args := map[string]any{"category": "bug_report"}
			if has {
				args["feedback"] = "The export button does nothing."
			}
			if res := callTool(t, cs, feedbackTool, args, nil); res.IsError {
				t.Fatalf("call matching the advertised schema rejected: %s", mustJSON(res))
			}
			if _, sent := r.props(t, "mcp_feedback_submitted")["feedback_text"]; sent != has {
				t.Fatalf("feedback_text sent = %v, want %v", sent, has)
			}
		})
	}
}

// The policy is fixed when the interaction starts: changing the caller's values afterwards, even
// concurrently with classification, changes nothing.
func TestInteractionPolicyIsCopiedAtStart(t *testing.T) {
	r := setupAI(t)
	p := &ContentCapture{IntentSummary: Ptr(true), Spans: &SpanCapture{Messages: Ptr(true)}}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", ContentCapture: p})
	done := make(chan struct{})
	go func() {
		*p.IntentSummary = false
		*p.Spans.Messages = false
		close(done)
	}()
	TrackClassification(ctx, TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "Refund question."})
	<-done
	TrackClassification(ctx, TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "Refund question."})
	for _, m := range r.named("ai_interaction_classified") {
		if _, ok := m.Properties["intent_summary"]; !ok {
			t.Fatalf("intent_summary withheld after the caller changed its policy: %v", m.Properties)
		}
	}
	if !i.capture.Spans.Messages {
		t.Fatal("span policy changed after start")
	}
	i.End(EndOptions{})
}

// WithContentCapture keeps its own copy: changing the caller's values later changes nothing.
func TestGlobalPolicyIsCopied(t *testing.T) {
	setupAI(t)
	p := ContentCapture{AnswerText: Ptr(true), Spans: &SpanCapture{HTTP: Ptr(true)}}
	if err := Configure(WithContentCapture(p)); err != nil {
		t.Fatal(err)
	}
	*p.AnswerText, *p.Spans.HTTP = false, false
	got := capture.Resolve(cfg().captureLevel())
	if !got.AnswerText || !got.Spans.HTTP {
		t.Fatalf("global policy follows the caller's values: %+v", got)
	}
}

// error_message follows the live policy at the final response: turning ErrorMessage off after
// the failure is held, but before the response is written, leaves it out; the event still goes out.
func TestHeldErrorMessageRechecksLivePolicy(t *testing.T) {
	setupAI(t, WithCaptureContent(true))
	s := failingServer("live-error-mcp")
	r := wrap(t, s, Options{})
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/call" {
				_ = Configure(WithContentCapture(ContentCapture{ErrorMessage: Ptr(false)}))
			}
			return res, err
		}
	})
	cs := connectMaybeObserved(t, s, true)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "lookup failed"}}); err == nil {
		t.Fatal("expected error")
	}
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	if p["tool_status"] != "error" {
		t.Fatalf("event: %v", p)
	}
	if _, ok := p["error_message"]; ok {
		t.Fatalf("error_message sent after ErrorMessage turned off: %v", p)
	}
}
