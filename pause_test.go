package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

var pauseCall = LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 2}

func TestSharedPauseSnapshotOrder(t *testing.T) {
	setupAI(t)
	b, err := os.ReadFile("spec/fixtures/pause-snapshot-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Calls [][2]string `json:"calls"`
		Seen  [][2]string `json:"seen_response_ids"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for _, pair := range fixture.Calls {
		i.RecordCall(LLMCall{Provider: pair[0], Model: "m", ResponseID: pair[1]})
	}
	snapshot, _ := i.Pause(PauseOptions{})
	if mustJSON(snapshot.SeenResponseIDs) != mustJSON(fixture.Seen) {
		t.Fatal(snapshot.SeenResponseIDs)
	}
	b, _ = json.Marshal(snapshot)
	var stored PausedInteraction
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	next, _ := resumed.Pause(PauseOptions{})
	if mustJSON(next.SeenResponseIDs) != mustJSON(fixture.Seen) {
		t.Fatal(next.SeenResponseIDs)
	}
}

func TestPauseDedupAcrossSegments(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			r := setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			call := pauseCall
			call.ResponseID, call.CostUSD = "same", Ptr(0.01)
			i.RecordCall(call)
			for range 2 {
				snapshot, _ := i.Pause(PauseOptions{})
				if restore {
					b, _ := json.Marshal(snapshot)
					var stored PausedInteraction
					if err := json.Unmarshal(b, &stored); err != nil {
						t.Fatal(err)
					}
					_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
				} else {
					i.Resume()
				}
				if i.RecordCall(call) {
					t.Fatal("duplicate counted after pause")
				}
			}
			call.ResponseID = "fresh"
			i.RecordCall(call)
			i.End(EndOptions{})
			for n, e := range r.named("ai_response_received") {
				count := 1
				if n == 1 {
					count = 0
				}
				if e.Properties["llm_call_count"] != count || e.Properties["total_tokens"] != 12*count {
					t.Fatal(e.Properties)
				}
			}
		})
	}
}

func TestPauseFlushesFinalizationBeforeJSONHandoff(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	finished := 0
	i.onBeforeEnd(func() { finished++; i.RecordCall(pauseCall) })
	snapshot, _ := i.Pause(PauseOptions{})
	if finished != 1 || lastResponse(t, r)["llm_call_count"] != 1 {
		t.Fatal("pause lost buffered call")
	}
	b, _ := json.Marshal(snapshot)
	var stored PausedInteraction
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	resumed.End(EndOptions{})
	if finished != 1 || lastResponse(t, r)["llm_call_count"] != 0 {
		t.Fatal(lastResponse(t, r))
	}
}

func TestPausedEndProtectsAdapterReentry(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.onBeforeEnd(func() { i.RecordCall(pauseCall); i.End(EndOptions{Status: StatusError}) })
	i.End(EndOptions{Status: StatusPaused})
	if !i.Paused() || i.Ended() || len(r.named("ai_response_received")) != 1 || lastResponse(t, r)["response_status"] != "paused" {
		t.Fatal("adapter reentry finalized paused end")
	}
}

func TestPauseMarksStateBeforeRedactorReentry(t *testing.T) {
	var i *Interaction
	r := setupAI(t, WithCaptureContent(true), WithRedactHook(func(text string) (string, error) {
		if !i.Paused() {
			t.Error("not paused during redaction")
		}
		i.End(EndOptions{Status: StatusError})
		return text, nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.RecordCall(pauseCall)
	i.Pause(PauseOptions{ResponseText: "approval"})
	if !i.Paused() || i.Ended() || len(r.named("ai_response_received")) != 1 {
		t.Fatal("reentry finalized pause")
	}
}

func TestPauseAfterSDKSelection(t *testing.T) {
	for _, reason := range []string{endReasonTTL, endReasonShutdown} {
		t.Run(reason, func(t *testing.T) {
			r := setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			var selected []*Interaction
			if reason == endReasonShutdown {
				selected = takeOpen()
			} else {
				open.mu.Lock()
				i.opened = time.Now().Add(-time.Hour)
				open.lastSweep = time.Time{}
				open.mu.Unlock()
				selected = expiredInteractions(time.Second)
			}
			if len(selected) != 1 {
				t.Fatal("interaction not selected")
			}
			i.Pause(PauseOptions{})
			for _, in := range selected {
				in.End(EndOptions{Status: StatusIncomplete, endReason: reason})
			}
			if !i.Paused() || i.Ended() || len(r.named("ai_response_received")) != 1 {
				t.Fatal("selected pause finalized")
			}
		})
	}
}

func TestPausedEndMasksOnceAndAppliesDisconnect(t *testing.T) {
	calls := 0
	r := setupAI(t, WithCaptureContent(true), WithRedactHook(func(text string) (string, error) { calls++; return text, nil }))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.End(EndOptions{Status: StatusPaused, ClientDisconnected: true, ResponseText: "approval"})
	if calls != 1 || lastResponse(t, r)["client_disconnected"] != true {
		t.Fatal(calls, lastResponse(t, r))
	}
}

func lastResponse(t *testing.T, r *recorder) map[string]any {
	t.Helper()
	got := r.named("ai_response_received")
	if len(got) == 0 {
		t.Fatalf("no ai_response_received: %v", r.names())
	}
	return got[len(got)-1].Properties
}

func TestPauseReturnsASnapshot(t *testing.T) {
	setupAI(t)
	_, in := StartInteraction(context.Background(), InteractionOptions{
		UserID: "u", ConversationID: "c", Feature: "f", AccountID: "acme", Intent: "refund", TraceID: "t",
	})
	in.RecordCall(pauseCall)
	paused, ok := in.Pause(PauseOptions{Reason: PauseApproval})
	if !ok || !in.Paused() || in.Ended() {
		t.Fatalf("pause: ok=%v paused=%v ended=%v", ok, in.Paused(), in.Ended())
	}
	b, err := json.Marshal(paused)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["paused_at"]; !has {
		t.Fatalf("no paused_at: %s", b)
	}
	delete(got, "paused_at")
	want := map[string]any{
		"interaction_id": in.ID(), "conversation_id": "c", "user_id": "u", "feature": "f", "trigger": "user",
		"account_id": "acme", "intent": "refund", "trace_id": "t", "pause_count": float64(1), "total_paused_ms": float64(0),
	}
	if mustJSON(got) != mustJSON(want) {
		t.Fatalf("snapshot:\n got %s\nwant %s", mustJSON(got), mustJSON(want))
	}
	in.End(EndOptions{})
}

func TestPauseSendsTextAndExtras(t *testing.T) {
	r := setupAI(t, WithCaptureContent(true))
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	in.Pause(PauseOptions{Reason: PauseHumanInput, ResponseText: "Which account?", ExtraProperties: map[string]any{"step": "ask", "response_status": "success"}})
	p := lastResponse(t, r)
	if p["response_status"] != "paused" || p["pause_reason"] != "human_input" || p["response_text"] != "Which account?" || p["step"] != "ask" {
		t.Fatalf("paused: %v", p)
	}
	in.End(EndOptions{})
}

func TestPausedInteractionOutlivesTTLAndShutdown(t *testing.T) {
	r := setupAI(t, WithLimits(Limits{InteractionTTL: 20 * time.Millisecond}))
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	in.Pause(PauseOptions{})
	if openCount() != 0 {
		t.Fatalf("paused interaction still on the open list (%d)", openCount())
	}
	time.Sleep(40 * time.Millisecond)
	answer(t, InteractionOptions{NoPromptEvent: true}, "")
	Shutdown(context.Background())
	if in.Ended() {
		t.Fatal("TTL or shutdown ended a paused interaction")
	}
	var statuses []any
	for _, e := range r.named("ai_response_received") {
		statuses = append(statuses, e.Properties["response_status"])
	}
	if mustJSON(statuses) != `["paused","success"]` {
		t.Fatalf("statuses: %v", statuses)
	}
	in.Resume()
	if openCount() != 1 {
		t.Fatalf("resumed interaction not open (%d)", openCount())
	}
	in.End(EndOptions{})
}

func TestRunInteractionAndTaskLeaveAPausedRunOpen(t *testing.T) {
	r := setupAI(t)
	var a, b *Interaction
	if err := RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error {
		a = i
		i.Pause(PauseOptions{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := RunTask(context.Background(), "job", TaskOptions{}, func(_ context.Context, i *Interaction) error {
		b = i
		i.Pause(PauseOptions{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a.Ended() || b.Ended() || len(r.named("ai_response_received")) != 2 {
		t.Fatalf("ended: %v %v, events %v", a.Ended(), b.Ended(), r.names())
	}
}

func TestPausedRunEndsWhenItsBodyFails(t *testing.T) {
	r := setupAI(t)
	boom := errors.New("boom")
	err := RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error {
		i.Pause(PauseOptions{})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	p := lastResponse(t, r)
	if p["response_status"] != "error" || p["error_type"] != "app_error" || p["pause_count"] != 1 {
		t.Fatalf("final: %v", p)
	}
}

func TestPausedTimeIsNotLatency(t *testing.T) {
	r := setupAI(t)
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	in.Pause(PauseOptions{})
	time.Sleep(50 * time.Millisecond)
	in.Resume()
	in.RecordCall(pauseCall)
	in.End(EndOptions{})
	p := lastResponse(t, r)
	paused, _ := p["total_paused_ms"].(int64)
	latency, _ := p["latency_ms"].(int64)
	if paused < 50 || latency >= 50 {
		t.Fatalf("total_paused_ms %v, latency_ms %v", p["total_paused_ms"], p["latency_ms"])
	}
}

func TestResumeFromASnapshot(t *testing.T) {
	r := setupAI(t)
	_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u", ConversationID: "c", TaskName: "loop"})
	first.RecordCall(pauseCall)
	paused, _ := first.Pause(PauseOptions{})
	b, _ := json.Marshal(paused)
	var stored PausedInteraction
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	stored.PausedAt -= 3000 // paused three seconds ago, in another process
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored, UserID: "other", PromptText: "ignored"})
	if resumed.ID() != first.ID() {
		t.Fatalf("resumed id %s, want %s", resumed.ID(), first.ID())
	}
	resumed.RecordCall(pauseCall)
	resumed.End(EndOptions{})
	if len(r.named("ai_user_prompt_created")) != 1 {
		t.Fatalf("prompts: %v", r.names())
	}
	events := r.named("ai_response_received")
	final := events[len(events)-1]
	p := final.Properties
	if final.UserId != "u" || p["interaction_id"] != first.ID() || p["conversation_id"] != "c" || p["task_name"] != "loop" ||
		p["pause_count"] != 1 || p["llm_call_count"] != 1 {
		t.Fatalf("final: %+v", final)
	}
	if ms, _ := p["total_paused_ms"].(int64); ms < 3000 {
		t.Fatalf("total_paused_ms %v", p["total_paused_ms"])
	}
}

func TestResumeFromAnUnusableSnapshot(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Resume: &PausedInteraction{PauseCount: 1}})
	if in.ID() == "" || logs.count("Resume:") == 0 {
		t.Fatalf("id %q, logs %s", in.ID(), logs.String())
	}
	in.End(EndOptions{})
}

func TestTaskStartedEvents(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		r := setupAI(t)
		_ = RunTask(context.Background(), "job", TaskOptions{}, func(context.Context, *Interaction) error { return nil })
		if len(r.named("ai_task_started")) != 0 {
			t.Fatal(r.names())
		}
	})
	t.Run("sent when a task starts", func(t *testing.T) {
		r := setupAI(t, WithTaskStartedEvents(true))
		_ = RunTask(context.Background(), "job", TaskOptions{TriggerSource: "schedule", AccountID: "acme"}, func(context.Context, *Interaction) error {
			if len(r.named("ai_task_started")) != 1 {
				t.Error("ai_task_started not sent before the run's work")
			}
			return nil
		})
		p := r.props(t, "ai_task_started")
		if len(p) != 5 || p["account_id"] != "acme" || p["trigger_source"] != "schedule" || p["task_name"] != "job" ||
			p["interaction_id"] == nil || p["conversation_id"] == nil {
			t.Fatalf("ai_task_started: %v", p)
		}
	})
}

func TestEndPausedIsResumable(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for range 2 {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 2, CostUSD: Ptr(0.01)})
		i.End(EndOptions{Status: StatusPaused, PauseReason: PauseApproval})
		if !i.Paused() || i.Ended() {
			t.Fatal("paused end must remain resumable")
		}
		i.Resume()
	}
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 2, CostUSD: Ptr(0.01)})
	i.End(EndOptions{})
	segments := r.named("ai_response_received")
	if len(segments) != 3 {
		t.Fatalf("segments: %d", len(segments))
	}
	for n, s := range segments {
		want := "paused"
		if n == 2 {
			want = "success"
		}
		if s.Properties["response_status"] != want || s.Properties["interaction_id"] != i.ID() || s.Properties["llm_call_count"] != 1 || s.Properties["total_cost_usd"] != 0.01 {
			t.Fatalf("segment: %v", s.Properties)
		}
	}
	if segments[2].Properties["pause_count"] != 2 {
		t.Fatal(segments[2].Properties)
	}
}

func TestResumeDoesNotInheritAnotherActor(t *testing.T) {
	r := setupAI(t)
	_, first := StartInteraction(context.Background(), InteractionOptions{AnonymousID: "visitor"})
	snapshot, _ := first.Pause(PauseOptions{})
	ctx := WithValues(context.Background(), ContextValues{UserID: "other", AccountID: "other-account", Feature: "other-feature"})
	_, resumed := StartInteraction(ctx, InteractionOptions{Resume: &snapshot})
	resumed.End(EndOptions{})
	final := r.named("ai_response_received")[1]
	if final.UserId != "" || final.AnonymousId != "visitor" {
		t.Fatalf("actor: %v", final)
	}
	if final.Properties["account_id"] != nil || final.Properties["feature"] != nil {
		t.Fatal(final.Properties)
	}
}

func TestRejectSnapshotWithoutPauseTimestampsAndCounters(t *testing.T) {
	setupAI(t)
	_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	snapshot := PausedInteraction{InteractionID: first.ID(), ConversationID: first.ConversationID()}
	_, resumed := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Resume: &snapshot})
	if resumed.ID() == first.ID() {
		t.Fatal("unusable snapshot accepted")
	}
	first.End(EndOptions{})
	resumed.End(EndOptions{})
}
