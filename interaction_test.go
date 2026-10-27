package rudderstackai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestInteractionLifecycle(t *testing.T) {
	r := setupAI(t, WithCaptureContent(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{
		UserID: "u1", Feature: "support", Surface: SurfaceWeb, Intent: "refund",
		AccountID: "acme", OnBehalfOf: "boss", TriggerSource: "slack", TaskName: "triage",
		TriggerEventID: "evt", ParentInteractionID: "parent", InteractionID: "i1", ConversationID: "c1",
		InputMethod: "text", PromptText: "mail me at a@b.com", AttachmentCount: Ptr(2),
		AttachmentTypes: []string{"IMAGE", "docx", "image", ""},
		Context:         map[string]any{"app": map[string]any{"build": "7"}},
	})
	if FromContext(ctx) != i || i.ID() != "i1" || i.ConversationID() != "c1" || i.Trigger() != TriggerUser {
		t.Fatal("interaction not in context")
	}
	if len(r.all()) != 0 {
		t.Fatalf("prompt sent before the first call: %v", r.names())
	}
	if !i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, OutputTokens: 5, Integration: IntegrationOpenAI, FinishReason: "stop"}) {
		t.Fatal("call not recorded")
	}
	prompt := r.props(t, "ai_user_prompt_created")
	want := map[string]any{
		"conversation_id": "c1", "interaction_id": "i1", "feature": "support", "surface": "web",
		"account_id": "acme", "on_behalf_of": "boss", "trigger_source": "slack", "task_name": "triage",
		"trigger_event_id": "evt", "parent_interaction_id": "parent", "input_method": "text",
		"attachment_count": 2, "attachment_types": []string{"image", "file"}, "intent": "refund",
		"prompt_text": "mail me at [email]",
	}
	if !reflect.DeepEqual(prompt, want) {
		t.Fatalf("prompt:\n%v\nwant\n%v", prompt, want)
	}
	i.SetIntent("billing")
	i.SetOutcome("resolved")
	i.SetTraceID("trace-1")
	i.RecordToolCallCount(2)
	i.RecordToolCallCount(0)
	i.End(EndOptions{ResponseText: "call 555-123-4567", ClientDisconnected: true})
	i.End(EndOptions{Status: StatusError})
	if !i.Ended() || i.RecordCall(LLMCall{Model: "late"}) {
		t.Fatal("a call after End was recorded")
	}
	resp := r.response(t)
	for k, v := range map[string]any{
		"response_status": "success", "trigger": "user", "primary_model": "gpt-4o", "primary_provider": "openai",
		"llm_call_count": 1, "tool_call_count": 2, "total_input_tokens": 10, "total_output_tokens": 5,
		"total_tokens": 15, "finish_reason": "stop", "intent": "billing", "outcome": "resolved",
		"trace_id": "trace-1", "client_disconnected": true, "response_text": "call [phone]",
	} {
		if !reflect.DeepEqual(resp[k], v) {
			t.Errorf("%s = %#v, want %#v", k, resp[k], v)
		}
	}
	for _, m := range r.all() {
		if integrationOf(m) != IntegrationOpenAI || m.UserId != "u1" {
			t.Fatalf("%s: integration %q user %q", m.Event, integrationOf(m), m.UserId)
		}
		if app, _ := m.Context.Extra["app"].(map[string]any); app["build"] != "7" {
			t.Fatalf("%s: context %v", m.Event, m.Context.Extra)
		}
	}
}

func TestHeldPromptFlushedAtEnd(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", AttachmentCount: Ptr(-1)})
	if len(r.all()) != 0 {
		t.Fatal("prompt not held")
	}
	i.End(EndOptions{})
	if got := r.names(); !slices.Equal(got, []string{"ai_user_prompt_created", "ai_response_received"}) {
		t.Fatal(got)
	}
	if _, has := r.props(t, "ai_user_prompt_created")["attachment_count"]; has {
		t.Fatal("negative attachment count sent")
	}
	if integrationOf(r.all()[0]) != IntegrationManual {
		t.Fatal("manual integration expected")
	}
}

func TestSystemAndNoPromptInteractions(t *testing.T) {
	r := setupAI(t)
	_, sys := StartInteraction(nil, InteractionOptions{Trigger: TriggerSystem}) //nolint:staticcheck // nil context is tolerated
	sys.End(EndOptions{})
	_, quiet := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	quiet.End(EndOptions{})
	if got := r.names(); !slices.Equal(got, []string{"ai_response_received", "ai_response_received"}) {
		t.Fatal(got)
	}
	if m := r.all()[0]; m.AnonymousId != systemAnonymousID || m.Properties["trigger"] != "system" {
		t.Fatalf("system event: %+v", m)
	}
}

func TestRunInteractionStatuses(t *testing.T) {
	for _, c := range []struct {
		err             error
		status, errType string
	}{
		{nil, "success", ""},
		{errors.New("boom"), "error", "app_error"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout", "timeout"},
		{context.Canceled, "cancelled", "cancelled"},
		// A provider signal (an HTTP status or error code) keeps api_error.
		{statusErr(500), "error", "api_error"},
		{fmt.Errorf("retry: %w", codedErr{"Mystery"}), "error", "api_error"},
		{fmt.Errorf("retry: %w", statusErr(429)), "error", "rate_limit"},
		{statusCodeErr(503), "error", "api_error"},
	} {
		r := setupAI(t)
		var inner *Interaction
		err := RunInteraction(context.Background(), InteractionOptions{UserID: "u", Feature: "f", IdentityFromCookies: Ptr(false)},
			func(ctx context.Context, i *Interaction) error {
				inner = i
				if FromContext(ctx) != i || valuesFrom(ctx).userID != "u" || valuesFrom(ctx).feature != "f" {
					t.Error("identity not in the context")
				}
				return c.err
			})
		if !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Fatalf("error not returned unchanged: %v", err)
		}
		resp := r.response(t)
		if resp["response_status"] != c.status || (c.errType != "" && resp["error_type"] != c.errType) || !inner.Ended() {
			t.Fatalf("%v: %v", c.err, resp)
		}
	}
}

func TestRunInteractionPanicIsReraised(t *testing.T) {
	r := setupAI(t)
	var inner *Interaction
	func() {
		defer func() {
			if p := recover(); p != "kaboom" {
				t.Fatalf("panic not re-raised: %v", p)
			}
		}()
		_ = RunInteraction(nil, InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error { //nolint:staticcheck // nil context is tolerated
			inner = i
			panic("kaboom")
		})
	}()
	if !inner.Ended() {
		t.Fatal("interaction not ended")
	}
	if resp := r.response(t); resp["response_status"] != "error" || resp["error_type"] != "app_error" {
		t.Fatal(resp)
	}
}

func TestRunInteractionPanicWithError(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{{statusErr(429), "rate_limit"}, {errors.New("bad state"), "app_error"}, {context.DeadlineExceeded, "timeout"}} {
		r := setupAI(t)
		func() {
			defer func() { _ = recover() }()
			_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(context.Context, *Interaction) error { panic(c.value) })
		}()
		if resp := r.response(t); resp["error_type"] != c.want {
			t.Errorf("%v: %v", c.value, resp)
		}
	}
}

// An error the app's own code returns after a successful call is app_error, not a provider failure.
func TestRunInteractionAppErrorAfterCall(t *testing.T) {
	r := setupAI(t)
	_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 2})
		return errors.New("context review failed")
	})
	resp := r.response(t)
	if resp["response_status"] != "error" || resp["error_type"] != "app_error" || resp["llm_call_count"] != 1 {
		t.Fatal(resp)
	}
}

// A wrapped call's own error keeps the type the call recorded, even without a provider signal.
func TestRunInteractionKeepsCallErrorType(t *testing.T) {
	r := setupAI(t)
	cause := errors.New("stream reset")
	_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", ErrorType: "connection_error"})
		i.noteCallError(cause, "connection_error")
		return fmt.Errorf("sdk: %w", cause)
	})
	if resp := r.response(t); resp["error_type"] != "connection_error" {
		t.Fatal(resp)
	}
}

func TestNoteCallErrorKeepsNewest(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	defer i.End(EndOptions{})
	first := errors.New("first")
	i.noteCallError(first, "timeout")
	i.noteCallError(nil, "timeout")
	for range maxCallErrors {
		i.noteCallError(errors.New("later"), "rate_limit")
	}
	if got := i.answerErrorType(first); got != "app_error" {
		t.Fatalf("evicted error kept: %s", got)
	}
}

func TestRunInteractionGoexitEndsWithoutPanic(t *testing.T) {
	r := setupAI(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(context.Context, *Interaction) error {
			defer runtime.Goexit() // as t.FailNow does
			return nil
		})
	}()
	<-done
	if resp := r.response(t); resp["response_status"] != "error" {
		t.Fatal(resp)
	}
}

func TestTraceIDFromContext(t *testing.T) {
	r := setupAI(t)
	type spanKey struct{}
	const tid = "4bf92f3577b34da6a3ce929d0e0e4736"
	if err := Configure(WithTraceIDFrom(func(ctx context.Context) string {
		s, _ := ctx.Value(spanKey{}).(string)
		return s
	})); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), spanKey{}, tid)

	_, fromSpan := StartInteraction(ctx, InteractionOptions{Trigger: TriggerSystem})
	fromSpan.End(EndOptions{})
	_, explicit := StartInteraction(ctx, InteractionOptions{Trigger: TriggerSystem, TraceID: "mine"})
	explicit.End(EndOptions{})
	// A trace present only when the first model call starts fills it too.
	_, late := StartInteraction(context.Background(), InteractionOptions{Trigger: TriggerSystem})
	late.callStarted(ctx, IntegrationOpenAI)
	late.End(EndOptions{})

	got := r.named("ai_response_received")
	if got[0].Properties["trace_id"] != tid || got[1].Properties["trace_id"] != "mine" || got[2].Properties["trace_id"] != tid {
		t.Fatalf("trace ids: %v %v %v", got[0].Properties["trace_id"], got[1].Properties["trace_id"], got[2].Properties["trace_id"])
	}
	if integrationOf(got[2]) != IntegrationOpenAI {
		t.Fatal("callStarted integration")
	}
}

func TestLLMCallEvents(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) {
		i.RecordCall(LLMCall{
			Provider: "openrouter", Model: "openai/gpt-4o-mini", RequestedModel: "openrouter/auto",
			InputTokens: 100, OutputTokens: 20, ReasoningTokens: Ptr(50), CachedInputTokens: 10,
			CacheWriteTokens: 5, CacheWrite1hTokens: 9, ServiceTier: "priority",
			ServerToolCalls: map[string]int{"web_search": 1, "none": 0}, ResponseID: "r1", FinishReason: "stop",
		})
		if i.RecordCall(LLMCall{Provider: "openai", Model: "x", ResponseID: "r1", AgentName: "planner"}) {
			t.Error("repeated response id recorded")
		}
		i.RecordCall(LLMCall{Provider: "anthropic", Model: "claude", InputTokens: -5, OutputTokens: 3, ReasoningTokens: Ptr(0)})
	})
	calls := r.named("ai_llm_response_received")
	if len(calls) != 2 {
		t.Fatalf("calls: %d", len(calls))
	}
	first := calls[0].Properties
	for k, v := range map[string]any{
		"provider": "openai", "model": "openai/gpt-4o-mini", "requested_model": "openrouter/auto",
		"reasoning_tokens": 20, "total_tokens": 120, "cached_input_tokens": 10, "cache_write_tokens": 5,
		"cache_write_1h_tokens": 5, "service_tier": "fast", "server_tool_call_count": 1, "response_id": "r1",
		"finish_reason": "stop",
	} {
		if !reflect.DeepEqual(first[k], v) {
			t.Errorf("%s = %#v, want %#v", k, first[k], v)
		}
	}
	if calls[1].Properties["input_tokens"] != 0 || calls[1].Properties["reasoning_tokens"] != 0 {
		t.Fatalf("sanitized: %v", calls[1].Properties)
	}
	resp := r.response(t)
	if resp["total_reasoning_tokens"] != 20 || resp["llm_call_count"] != 2 {
		t.Fatal(resp)
	}
	b := resp["usage_breakdown"].([]map[string]any)
	if b[0]["agent_name"] != "planner" || b[0]["cache_write_1h_tokens"] != 5 || b[0]["reasoning_tokens"] != 20 {
		t.Fatalf("breakdown: %v", b[0])
	}
}

func TestStatusDerivation(t *testing.T) {
	cases := []struct {
		name   string
		calls  []LLMCall
		end    EndOptions
		status string
		check  func(t *testing.T, p map[string]any)
	}{
		{name: "last call failed", calls: []LLMCall{{Model: "a"}, {Model: "b", ErrorType: "rate_limit"}}, status: "error", check: func(t *testing.T, p map[string]any) {
			if p["error_type"] != "rate_limit" || p["primary_model"] != "a" {
				t.Fatal(p)
			}
		}},
		{name: "retry succeeded", calls: []LLMCall{{Model: "a", ErrorType: "timeout"}, {Model: "b"}}, status: "success"},
		{name: "only failures", calls: []LLMCall{{Model: "a", ErrorType: "api_error", CallCount: 3}}, status: "error", check: func(t *testing.T, p map[string]any) {
			if p["primary_model"] != "a" || p["llm_call_count"] != 3 {
				t.Fatal(p)
			}
			if b := p["usage_breakdown"].([]map[string]any); b[0]["error_count"] != 3 {
				t.Fatal(b)
			}
		}},
		{name: "explicit error count", calls: []LLMCall{{Model: "a", ErrorType: "api_error", CallCount: 3, ErrorCount: Ptr(5)}, {Model: "a", CallCount: 2, ErrorCount: Ptr(1)}}, status: "success", check: func(t *testing.T, p map[string]any) {
			if b := p["usage_breakdown"].([]map[string]any); b[0]["error_count"] != 4 || b[0]["call_count"] != 5 {
				t.Fatal(b)
			}
		}},
		{name: "length", calls: []LLMCall{{Model: "a", FinishReason: "length"}}, status: "incomplete", check: func(t *testing.T, p map[string]any) {
			if p["error_type"] != "length" {
				t.Fatal(p)
			}
		}},
		{name: "content filter", calls: []LLMCall{{Model: "a", FinishReason: "content_filter"}}, status: "error"},
		{name: "cancelled without calls", end: EndOptions{Status: StatusCancelled}, status: "cancelled", check: func(t *testing.T, p map[string]any) {
			if _, has := p["total_tokens"]; has || p["error_type"] != "cancelled" {
				t.Fatal(p)
			}
		}},
		{name: "invalid status", calls: []LLMCall{{Model: "a"}}, end: EndOptions{Status: "weird"}, status: "success"},
		{name: "paused", end: EndOptions{Status: StatusPaused, PauseReason: PauseApproval}, status: "paused", check: func(t *testing.T, p map[string]any) {
			if p["pause_reason"] != "approval" {
				t.Fatal(p)
			}
		}},
		{name: "paused bad reason", end: EndOptions{Status: StatusPaused, PauseReason: "lunch"}, status: "paused", check: func(t *testing.T, p map[string]any) {
			if p["pause_reason"] != "other" {
				t.Fatal(p)
			}
		}},
		{name: "extra properties", end: EndOptions{ExtraProperties: map[string]any{"plan": "pro", "interaction_id": "x", "total_cost_usd": 9, "tool_breakdown": 1}}, status: "success", check: func(t *testing.T, p map[string]any) {
			if p["plan"] != "pro" || p["interaction_id"] == "x" {
				t.Fatal(p)
			}
			for _, k := range []string{"total_cost_usd", "tool_breakdown"} {
				if _, has := p[k]; has {
					t.Fatalf("reserved %s kept", k)
				}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := setupAI(t)
			logs := captureLogs(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			for _, call := range c.calls {
				i.RecordCall(call)
			}
			i.End(c.end)
			p := r.response(t)
			if p["response_status"] != c.status {
				t.Fatalf("status %v, want %s", p["response_status"], c.status)
			}
			if c.check != nil {
				c.check(t, p)
			}
			if c.end.Status == "weird" && logs.count("invalid status") != 1 {
				t.Fatalf("warning: %s", logs)
			}
			if c.end.PauseReason == "lunch" && logs.count("invalid pause reason") != 1 {
				t.Fatalf("warning: %s", logs)
			}
		})
	}
}

func TestIntegrationHooks(t *testing.T) {
	r := setupAI(t)
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	adapter := &sink{}
	got, ephemeral := forCall(ctx, IntegrationAnthropic, nil)
	if got != i || ephemeral {
		t.Fatal("active interaction not used")
	}
	if len(r.names()) != 1 {
		t.Fatalf("held prompt not flushed at the call: %v", r.names())
	}
	i.firstTokenAt(i.started.Add(30 * time.Millisecond))
	i.firstTokenAt(i.started.Add(10 * time.Millisecond))
	i.firstTokenAt(i.started.Add(-time.Second))
	ran := 0
	i.onBeforeEnd(func() { ran++ })
	i.onBeforeEnd(func() { panic("hook") })
	i.hintOutcome(StatusCancelled, "")
	i.End(EndOptions{})
	i.onBeforeEnd(func() { ran++ })
	p := r.response(t)
	if p["response_status"] != "cancelled" || p["time_to_first_token_ms"] != int64(10) || ran != 1 {
		t.Fatalf("%v ran=%d", p, ran)
	}
	if integrationOf(r.all()[1]) != IntegrationAnthropic {
		t.Fatal("integration of the first call")
	}

	// A hint is dropped once a call is recorded after it.
	r.reset()
	_, j := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	j.hintOutcome(StatusError, "api_error")
	j.RecordCall(LLMCall{Model: "m"})
	j.End(EndOptions{})
	if p := r.response(t); p["response_status"] != "success" {
		t.Fatal(p)
	}

	// After End, a call opens an ephemeral child interaction.
	child, ephemeral := forCall(ctx, IntegrationOpenAI, adapter)
	if child == nil || !ephemeral || child.parent != i.id || child.trigger != TriggerSystem {
		t.Fatalf("ephemeral: %+v", child)
	}
	child.RecordCall(LLMCall{Model: "m"})
	child.End(EndOptions{})
	if got := adapter.names(); !slices.Equal(got, []string{"ai_response_received"}) {
		t.Fatalf("adapter client: %v", got)
	}
	// Identity in the context makes an orphan call user-triggered.
	orphan, _ := forCall(WithValues(context.Background(), ContextValues{UserID: "u"}), "", nil)
	if orphan.trigger != TriggerUser {
		t.Fatal("orphan trigger")
	}
	// An integration's client is adopted when nothing more specific is set; the held prompt goes
	// there too.
	r.reset()
	ctx2, k := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	other := &sink{}
	if got, _ := forCall(ctx2, IntegrationOpenAI, other); got != k {
		t.Fatal("active interaction not used")
	}
	k.End(EndOptions{})
	if got := other.names(); !slices.Equal(got, []string{"ai_user_prompt_created", "ai_response_received"}) || len(r.names()) != 0 {
		t.Fatalf("adopted client: %v, global %v", got, r.names())
	}
	if c, _ := forCall(withUntracked(context.Background()), "", nil); c != nil {
		t.Fatal("untracked call tracked")
	}
	if c, _ := forCall(nil, "", nil); c == nil { //nolint:staticcheck // nil context is tolerated
		t.Fatal("nil context")
	}
}

func TestHooksReachTheInteraction(t *testing.T) {
	r := setupAI(t, WithEvaluator(func(context.Context, EvaluationTurn) ([]Score, error) { return nil, nil }))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	got, ephemeral := hook.ForCall(ctx, IntegrationOpenAI, nil)
	if got != any(i) || ephemeral || !hook.KeepsText(got) {
		t.Fatal("ForCall")
	}
	hook.KeepMessages(got, "earlier prompt")
	hook.FirstToken(got, time.Now())
	hook.OnBeforeEnd(got, func() { hook.HintOutcome(got, string(StatusTimeout), "timeout") })
	i.End(EndOptions{})
	if p := r.response(t); p["response_status"] != "timeout" || p["error_type"] != "timeout" {
		t.Fatal(p)
	}
	if none, _ := hook.ForCall(withUntracked(ctx), "", nil); none != nil || !hook.Untracked(withUntracked(ctx)) || hook.Untracked(nil) { //nolint:staticcheck // nil context is tolerated
		t.Fatal("untracked")
	}
	if hook.StatusForErrorType("cancelled") != "cancelled" {
		t.Fatal("StatusForErrorType")
	}
	hook.RegisterErrorClassifier(func(error) (string, bool) { return "", false })
	hook.Reset()
	if cfg().analytics != nil {
		t.Fatal("Reset")
	}
}

func TestToolEventRules(t *testing.T) {
	r := setupAI(t, WithToolBreakdown(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
		"search": {Event: "Search Ran", Properties: func(args map[string]any, result any) map[string]any {
			return map[string]any{"query": args["q"], "hits": result, "tool_name": "spoof", "interaction_id": "spoof"}
		}},
		"mcp__crm__lookup": {Event: "CRM Lookup"},
		"skip":             {Event: "Never", When: func(map[string]any, any) bool { return false }},
		"explode":          {Event: "Exploded", When: func(map[string]any, any) bool { panic("when") }},
		"bad props":        {Event: "Bad Props", Properties: func(map[string]any, any) map[string]any { panic("props") }},
		"pending":          {Event: "Pending Tool"},
	}}))
	run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) {
		i.RecordToolCalls(ToolCall{ID: "c1", Name: "search", Args: map[string]any{"q": "shoes"}, AgentName: "a"})
		if len(r.named("Search Ran")) != 0 {
			t.Error("an unsettled call fired")
		}
		i.RecordToolResult("c1", ToolResult{Result: 3, Duration: Ptr(5 * time.Millisecond)})
		i.RecordToolResult("c1", ToolResult{Error: Ptr(true), ErrorType: "weird"})
		i.RecordToolCalls(ToolCall{ID: "c1", Name: "search"})
		i.RecordToolResult("", ToolResult{Error: Ptr(true)})
		i.RecordToolResult("unknown", ToolResult{Error: Ptr(true)})
		i.RecordToolCalls(
			ToolCall{ID: "c2", Name: "mcp__crm__lookup", Error: Ptr(false), Duration: Ptr(time.Duration(0))},
			ToolCall{Name: "skip", Error: Ptr(false)},
			ToolCall{Name: "explode", Error: Ptr(false)},
			ToolCall{Name: "bad props", Error: Ptr(true), ErrorType: "timeout", Duration: Ptr(2 * time.Millisecond)},
			ToolCall{Name: "pending", Duration: Ptr(time.Duration(0))},
			ToolCall{Name: "untracked", Error: Ptr(false), MCPServer: "files", ParentTool: "agent"},
		)
	})
	search := r.props(t, "Search Ran")
	if search["query"] != "shoes" || search["hits"] != 3 || search["tool_name"] != "search" || search["tool_call_id"] != "c1" ||
		search["tool_status"] != "unknown" || search["tool_duration_ms"] != int64(5) || search["agent_name"] != "a" || search["trigger"] != "user" {
		t.Fatalf("search: %v", search)
	}
	if crm := r.props(t, "CRM Lookup"); crm["tool_name"] != "lookup" || crm["tool_status"] != "success" {
		t.Fatalf("crm: %v", crm)
	}
	if bad := r.props(t, "Bad Props"); bad["tool_status"] != "error" || bad["tool_duration_ms"] != int64(2) {
		t.Fatalf("bad props: %v", bad)
	}
	if pending := r.props(t, "Pending Tool"); pending["tool_status"] != "unknown" {
		t.Fatalf("pending: %v", pending)
	} else if _, has := pending["tool_duration_ms"]; has {
		t.Fatal("0 duration sent for an unknown outcome")
	}
	if len(r.named("Never"))+len(r.named("Exploded")) != 0 {
		t.Fatal("When rules ignored")
	}
	resp := r.response(t)
	if resp["tool_call_count"] != 7 {
		t.Fatalf("tool count: %v", resp["tool_call_count"])
	}
	b := resp["tool_breakdown"].([]map[string]any)
	names := []string{}
	for _, e := range b {
		names = append(names, e["tool_name"].(string))
	}
	// Sorted by agent, MCP server, tool name, parent tool.
	if !slices.Equal(names, []string{"bad props", "explode", "pending", "skip", "lookup", "untracked", "search"}) {
		t.Fatalf("order: %v", names)
	}
	searchEntry := b[6]
	if searchEntry["error_count"] != 1 || !reflect.DeepEqual(searchEntry["error_types"], map[string]any{"api_error": 1}) || searchEntry["duration_ms"] != int64(5) {
		t.Fatalf("search entry: %v", searchEntry)
	}
	if b[5]["mcp_server"] != "files" || b[5]["parent_tool"] != "agent" || b[2]["error_count"] != nil {
		t.Fatalf("entries: %v", b)
	}
}

func TestEventRulesValidation(t *testing.T) {
	setupAI(t)
	for _, rules := range []map[string]ToolEventRule{
		{"t": {Event: ""}},
		{"t": {Event: "ai_custom"}},
		{"t": {Event: "mcp_custom"}},
	} {
		if err := Configure(WithCaptureContent(true), WithEventRules(EventRules{Tools: rules})); err == nil {
			t.Fatalf("accepted %v", rules)
		}
	}
	if err := Configure(WithAzureDeploymentModels(map[string]string{"": "m"})); err == nil {
		t.Fatal("empty deployment accepted")
	}
	if cfg().captureContent {
		t.Fatal("a failed Configure changed a setting")
	}
	if err := Configure(nil, WithRedaction(false), WithServerFeedback(false)); err != nil {
		t.Fatal(err)
	}
	if c := cfg(); c.redact || c.serverFeedback {
		t.Fatal("options not applied")
	}
}

func TestToolBreakdownHelpers(t *testing.T) {
	if name, server := normalizeTool("mcp__srv__do", "explicit"); name != "do" || server != "explicit" {
		t.Fatal(name, server)
	}
	for _, n := range []string{"mcp__", "mcp____x", "mcp__srv__"} {
		if name, _ := normalizeTool(n, ""); name != n {
			t.Fatalf("%q split", n)
		}
	}
	if compareUTF16("￿", "\U0001F600") <= 0 {
		t.Fatal("UTF-16 order: a surrogate pair sorts before U+FFFF")
	}
	if toolStatus(nil) != "unknown" || toolStatus(Ptr(true)) != "error" || toolStatus(Ptr(false)) != "success" {
		t.Fatal("toolStatus")
	}
	if normalizeToolErrorType("bogus") != "api_error" || normalizeToolErrorType("denied") != "denied" {
		t.Fatal("tool error types")
	}
}

func TestConcurrentRecording(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithToolBreakdown(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"t": {Event: "Tool Used"}}}))
	const n = 64
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	var wg sync.WaitGroup
	for k := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(k)
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1, OutputTokens: 1, ResponseID: id})
			i.RecordToolCalls(ToolCall{ID: id, Name: "t"})
			i.RecordToolResult(id, ToolResult{Error: Ptr(false)})
			i.SetIntent("x")
			i.firstTokenAt(time.Now())
			_ = i.keepsText()
			i.keepMessages([]any{})
		}()
	}
	wg.Wait()
	i.End(EndOptions{})
	p := r.response(t)
	if p["llm_call_count"] != n || p["tool_call_count"] != n || p["total_tokens"] != 2*n {
		t.Fatal(p)
	}
	if len(r.named("Tool Used")) != n || len(r.named("ai_llm_response_received")) != n {
		t.Fatal("per-call events")
	}
}

func TestWithValuesInheritance(t *testing.T) {
	r := setupAI(t)
	ctxClient := &sink{}
	ctx := WithValues(nil, ContextValues{UserID: "u", Feature: "f", Surface: SurfaceSlack, AccountID: "acct", Context: map[string]any{"k": "v"}}) //nolint:staticcheck // nil context is tolerated
	ctx = WithValues(ctx, ContextValues{ConversationID: "conv", OnBehalfOf: "boss", Analytics: ctxClient, AnonymousID: "anon"})
	cv := valuesFrom(ctx)
	if cv.userID != "u" || cv.feature != "f" || cv.conversationID != "conv" || cv.surface != SurfaceSlack {
		t.Fatalf("not inherited: %+v", cv)
	}
	_, i := StartInteraction(ctx, InteractionOptions{Surface: "bogus"})
	i.End(EndOptions{})
	if len(r.all()) != 0 {
		t.Fatal("context client ignored")
	}
	p := ctxClient.named("ai_response_received")[0]
	if p.UserId != "u" || p.AnonymousId != "anon" || p.Properties["conversation_id"] != "conv" || p.Properties["surface"] != "slack" ||
		p.Properties["account_id"] != "acct" || p.Properties["on_behalf_of"] != "boss" || p.Properties["feature"] != "f" || p.Context.Extra["k"] != "v" {
		t.Fatalf("%+v", p)
	}
	explicit := &sink{}
	_, j := StartInteraction(ctx, InteractionOptions{Analytics: explicit, Feature: "g", Trigger: TriggerSystem})
	j.End(EndOptions{})
	if got := explicit.named("ai_response_received"); len(got) != 1 || got[0].Properties["feature"] != "g" {
		t.Fatal("explicit client")
	}
	if FromContext(nil) != nil || valuesFrom(nil) != nil { //nolint:staticcheck // nil context is tolerated
		t.Fatal("nil context")
	}
}

func TestClientFailuresNeverReachTheCaller(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	failing := &sink{err: errors.New("queue full")}
	if err := Configure(WithAnalytics(failing)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) { i.RecordCall(LLMCall{Model: "m"}) })
	}
	if logs.count("Enqueue failed") != 1 || len(failing.names()) != 4 {
		t.Fatalf("warnings: %s events %v", logs, failing.names())
	}
	if err := Configure(WithAnalytics(&sink{panics: true})); err != nil {
		t.Fatal(err)
	}
	run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) { i.RecordCall(LLMCall{Model: "m"}) })
	TrackAction(context.Background(), TrackActionOptions{InteractionID: "i", ConversationID: "c", ActionType: ActionShared})

	// An uncomparable client still works and is never remembered for Flush.
	if err := Configure(WithAnalytics(sliceClient{1})); err != nil {
		t.Fatal(err)
	}
	run(t, InteractionOptions{}, func(context.Context, *Interaction) {})
	if !sameClient(nil, nil) || sameClient(sliceClient{1}, nil) || sameClient(sliceClient{1}, sliceClient{1}) {
		t.Fatal("sameClient")
	}
}

func TestNoClientWarnsOnce(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	if err := Configure(WithAnalytics(nil)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		run(t, InteractionOptions{}, func(_ context.Context, i *Interaction) { i.RecordCall(LLMCall{Model: "m"}) })
	}
	if logs.count("no RudderStack client configured") != 1 {
		t.Fatalf("warnings: %s", logs)
	}
	// Configuring another client re-arms the warning.
	if err := Configure(WithAnalytics(&sink{})); err != nil {
		t.Fatal(err)
	}
	if err := Configure(WithAnalytics(nil)); err != nil {
		t.Fatal(err)
	}
	run(t, InteractionOptions{}, func(context.Context, *Interaction) {})
	if logs.count("no RudderStack client configured") != 2 {
		t.Fatalf("warnings: %s", logs)
	}
}

func TestWrapperLibraryNesting(t *testing.T) {
	r := setupAI(t)
	outer := map[string]any{"name": "my-wrapper"}
	run(t, InteractionOptions{Context: map[string]any{"wrapperLibrary": outer}, NoPromptEvent: true}, func(context.Context, *Interaction) {})
	w := r.all()[0].Context.Extra["wrapperLibrary"].(map[string]any)
	if w["name"] != LibraryName || !reflect.DeepEqual(w["wrapperLibrary"], outer) || w["specVersion"] != SpecVersion {
		t.Fatal(w)
	}
}

type codedErr struct{ code string }

func (e codedErr) Error() string     { return e.code }
func (e codedErr) ErrorCode() string { return e.code }

type statusErr int

func (e statusErr) Error() string       { return "status" }
func (e statusErr) HTTPStatusCode() int { return int(e) }

type statusCodeErr int

func (e statusCodeErr) Error() string   { return "status code" }
func (e statusCodeErr) StatusCode() int { return int(e) }

type netErr struct{ timeout bool }

func (e netErr) Error() string   { return "net" }
func (e netErr) Timeout() bool   { return e.timeout }
func (e netErr) Temporary() bool { return false }

var errClassified = errors.New("classified by an integration")

func TestErrorTypeOf(t *testing.T) {
	registerErrorClassifier(func(err error) (string, bool) {
		if errors.Is(err, errClassified) {
			return "denied", true
		}
		if err.Error() == "panic please" {
			panic("classifier")
		}
		return "", false
	})
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("x: %w", errClassified), "denied"},
		{errors.New("panic please"), "api_error"},
		{codedErr{"ThrottlingException"}, "rate_limit"},
		{codedErr{"insufficient_quota"}, "rate_limit"},
		{codedErr{"RequestTimeout"}, "timeout"},
		{codedErr{"AccessDeniedException"}, "auth_error"},
		{codedErr{"ValidationException"}, "invalid_request"},
		{codedErr{"Mystery"}, "api_error"},
		{fmt.Errorf("retry: %w", statusErr(429)), "rate_limit"},
		{statusErr(504), "timeout"},
		{statusErr(403), "auth_error"},
		{statusErr(422), "invalid_request"},
		{statusErr(500), "api_error"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{netErr{timeout: true}, "timeout"},
		{netErr{}, "connection_error"},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, "connection_error"},
	} {
		if got := ErrorTypeOf(c.err); got != c.want {
			t.Errorf("ErrorTypeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	for status, want := range map[string]ResponseStatus{"timeout": StatusTimeout, "cancelled": StatusCancelled, "max_turns": StatusIncomplete, "length": StatusIncomplete, "api_error": StatusError} {
		if got := statusForErrorType(status); got != want {
			t.Errorf("%s: %s", status, got)
		}
	}
}
