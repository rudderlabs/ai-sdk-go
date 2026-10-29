package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestReviewUnsetToolFields(t *testing.T) {
	for _, padding := range []int{0, 40000} {
		r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{
			"search": {Event: "searched", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"mcp_server": "custom", "result_size_bytes": 7, "padding": strings.Repeat("x", padding)}
			}},
		}}))
		run(t, InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) { i.RecordToolCalls(ToolCall{Name: "search"}) })
		p := r.props(t, "searched")
		if padding == 0 && (p["mcp_server"] != "custom" || p["result_size_bytes"] != 7) {
			t.Fatal(p)
		}
		if padding > 0 && (p["mcp_server"] != nil || p["result_size_bytes"] != nil) {
			t.Fatal(p)
		}
	}
}

func TestReviewInvalidCreditHookReports(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		seen := &seenErrors{}
		setupAI(t, WithOnError(seen.hook))
		if priceCredits(func(CreditCall) (*float64, error) { return &value, nil }, CreditCall{}) != nil {
			t.Fatal("accepted invalid credits")
		}
		if got := seen.all(); len(got) != 1 || got[0].Kind != KindUserCallback {
			t.Fatal(got)
		}
	}
}

func TestReviewDuplicateWrapperMetadata(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", InputTokens: 5})
	if i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", CallPurpose: "answer", BYOK: Ptr(true), FallbackUsed: Ptr(false), Credits: Ptr(2.0)}) {
		t.Fatal("duplicate counted")
	}
	if i.callCount != 1 || i.calls[0].callPurpose != "answer" || i.calls[0].byok == nil || i.calls[0].fallbackUsed == nil || i.calls[0].credits == nil || *i.calls[0].credits != 2 {
		t.Fatal(i.calls[0])
	}
	i.End(EndOptions{})
	if r.props(t, "ai_response_received")["total_credits"] != 2.0 {
		t.Fatal(r.props(t, "ai_response_received"))
	}
}

func TestReviewDuplicateInvalidCreditsClear(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", Credits: Ptr(2.0)})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r", Credits: Ptr(-1.0)})
	if i.calls[0].credits != nil {
		t.Fatal("stale credits retained")
	}
	i.End(EndOptions{})
}

func TestReviewSampleOwned(t *testing.T) {
	for _, rates := range []map[string]float64{nil, {"event": 1}} {
		p := map[string]any{"sample_rate": 0.1, "other": 1}
		out, kept := sample("event", p, rates)
		if !kept || out["sample_rate"] != nil || p["sample_rate"] != 0.1 {
			t.Fatal(out, p)
		}
	}
}
func TestReviewSamplingToolBudget(t *testing.T) {
	for _, mcp := range []bool{false, true} {
		for _, supplied := range []bool{false, true} {
			r := setupAI(t, WithSampleRates(map[string]float64{"searched": 0.5}))
			id := "3f2c9a4e-8b1d-4c6f-9e2a-7d5b1c0e4a93"
			base := map[string]any{"interaction_id": id, "tool_name": "search", "tool_status": "success"}
			if !mcp {
				base["conversation_id"] = "c"
				base["trigger"] = "user"
			}
			custom := map[string]any{"padding": ""}
			if supplied {
				custom["sample_rate"] = 0.1
			}
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			for k, v := range custom {
				p[k] = v
			}
			custom["padding"] = strings.Repeat("x", maxPropertiesBytes-propertiesBytes(p))
			rule := ToolEventRule{Event: "searched", Properties: func(map[string]any, any) map[string]any { return custom }}
			if mcp {
				rc := &reqCtx{opts: &options{Options: Options{Analytics: r}, rules: map[string]ToolEventRule{"search": rule}}, base: base, toolName: "search", tool: &toolState{}}
				rc.declaredEvent(nil, "success")
			} else {
				if err := Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": rule}})); err != nil {
					t.Fatal(err)
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", ConversationID: "c", InteractionID: id, NoPromptEvent: true})
				i.RecordToolCalls(ToolCall{Name: "search", Error: Ptr(false)})
				i.End(EndOptions{})
			}
			got := r.props(t, "searched")
			if propertiesBytes(got) > maxPropertiesBytes || got["sample_rate"] != 0.5 {
				t.Errorf("mcp=%v supplied=%v bytes=%d stamp=%v", mcp, supplied, propertiesBytes(got), got["sample_rate"])
			}
		}
	}
}

func TestReviewResumeContext(t *testing.T) {
	for _, trigger := range []Trigger{TriggerUser, TriggerSystem} {
		setupAI(t)
		ctx := WithValues(context.Background(), ContextValues{UserID: "worker", AnonymousID: "worker-anon", ConversationID: "worker-conv", Feature: "worker-feature", Surface: SurfaceWeb, AccountID: "worker-account", OnBehalfOf: "worker-owner", AgentVersion: "worker-version", Variant: "worker-variant", Cookies: "ajs_anonymous_id=worker"})
		snapshot := PausedInteraction{InteractionID: "restored", ConversationID: "saved", UserID: "saved-user", Trigger: trigger, PausedAt: time.Now().UnixMilli(), PauseCount: 1}
		err := RunInteraction(ctx, InteractionOptions{Resume: &snapshot}, func(ctx context.Context, i *Interaction) error {
			cv := valuesFrom(ctx)
			if cv.userID != "saved-user" || cv.anonymousID != "" || cv.conversationID != "saved" || cv.feature != "" || cv.surface != "" || cv.accountID != "" || cv.onBehalfOf != "" || cv.agentVersion != "" || cv.variant != "" || len(cv.cookies) != 0 {
				t.Errorf("restored scope: %+v", cv)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestReviewDuplicateDoesNotResume(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	call := LLMCall{Provider: "openai", Model: "m", ResponseID: "r"}
	i.RecordCall(call)
	i.Pause(PauseOptions{})
	if i.RecordCall(call) || !i.Paused() {
		t.Fatal("duplicate resumed paused interaction")
	}
	if len(r.named("ai_response_received")) != 1 {
		t.Fatal(r.names())
	}
	i.Resume()
	i.End(EndOptions{})
}
func TestReviewCreditsPauseAcceptsAndResumes(t *testing.T) {
	var i *Interaction
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { i.Pause(PauseOptions{}); return Ptr(2.0), nil }))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	if !i.RecordCall(LLMCall{Model: "m"}) || i.Paused() {
		t.Fatal("accepted call remained paused")
	}
	i.End(EndOptions{})
	if lastResponse(t, r)["total_credits"] != 2.0 {
		t.Fatal(lastResponse(t, r))
	}
}
func TestReviewPendingAgentRestoration(t *testing.T) {
	setupAI(t)
	snapshot := PausedInteraction{InteractionID: "restored", ConversationID: "saved", UserID: "u", Trigger: TriggerUser, PausedAt: time.Now().UnixMilli(), PauseCount: 1, SeenResponseIDs: [][2]string{{"openai", "completed"}, {"openai", "pending"}}, PendingCalls: []map[string]any{{"provider": "openai", "model": "m", "response_id": "pending", "call_count": 1, "last": true, "last_ok": true}}}
	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var stored PausedInteraction
	if err = json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	if i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "pending", AgentName: "agent"}) {
		t.Fatal("duplicate accepted")
	}
	if len(i.calls) != 1 || i.calls[0].agentName != "agent" || i.seen["openai\x00completed"] != nil {
		t.Fatal("pending agent enrichment lost")
	}
	i.End(EndOptions{})
}

type reviewReentrantResult struct {
	i       *Interaction
	blocked bool
}

func (r *reviewReentrantResult) MarshalJSON() ([]byte, error) {
	done := make(chan struct{})
	go func() { r.i.SetOutcome("serialized"); close(done) }()
	select {
	case <-done:
		return []byte(`{"ok":true}`), nil
	case <-time.After(200 * time.Millisecond):
		r.blocked = true
		return nil, errors.New("interaction lock held")
	}
}
func TestReviewResultSerializationUnlocked(t *testing.T) {
	r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": {Event: "searched"}}}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	result := &reviewReentrantResult{i: i}
	i.RecordToolCalls(ToolCall{Name: "search", Result: result})
	if result.blocked || r.props(t, "searched")["result_size_bytes"] != 11 {
		t.Fatal("result serialization held lock or lost byte size")
	}
	i.End(EndOptions{})
}
