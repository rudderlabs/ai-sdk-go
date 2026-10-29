package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

var pauseCall = LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 10, OutputTokens: 2}

func TestRestoreSharedPendingUsageWithoutReplayingCallEvents(t *testing.T) {
	for _, unpriced := range []bool{false, true} {
		t.Run(fmt.Sprint(unpriced), func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithToolBreakdown(true))
			b, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Snapshot PausedInteraction
				Expected map[string]any
			}
			if err := json.Unmarshal(b, &fixture); err != nil {
				t.Fatal(err)
			}
			if unpriced {
				fixture.Snapshot.PendingCalls[0]["unpriced"] = true
				delete(fixture.Expected, "total_cost_usd")
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &fixture.Snapshot})
			if len(r.names()) != 0 {
				t.Fatal("restoration replayed events", r.names())
			}
			call := pauseCall
			call.ResponseID = "late"
			if i.RecordCall(call) {
				t.Fatal("restoration lost response ID history")
			}
			i.End(EndOptions{})
			got := lastResponse(t, r)
			if unpriced && got["total_cost_usd"] != nil {
				t.Fatal("partial pending cost became a known total", got)
			}
			for key, want := range fixture.Expected {
				if mustJSON(got[key]) != mustJSON(want) {
					t.Errorf("%s: got %v, want %v", key, got[key], want)
				}
			}
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("restoration replayed calls")
			}

		})
	}
}

func TestPauseCallbackReentryReturnsImmediately(t *testing.T) {
	for _, source := range []string{"finalizer", "redactor", "disconnect"} {
		t.Run(source, func(t *testing.T) {
			var i *Interaction
			entered, returned := make(chan struct{}), make(chan bool, 1)
			reenter := func() {
				close(entered)
				_, ok := i.Pause(PauseOptions{})
				returned <- ok
			}
			r := setupAI(t, WithCaptureContent(true), WithRedactHook(func(text string) (string, error) {
				if source == "redactor" {
					reenter()
				}
				return text, nil
			}))
			_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
			if source == "finalizer" {
				i.onBeforeEnd(reenter)
			}
			if source == "disconnect" {
				i.disconnected = func() bool { reenter(); return false }
			}
			done := make(chan bool, 1)
			go func() { _, ok := i.Pause(PauseOptions{ResponseText: "approval"}); done <- ok }()
			<-entered
			select {
			case ok := <-returned:
				if ok {
					t.Fatal("reentrant pause exposed an unfinished snapshot")
				}
			case <-time.After(time.Second):
				t.Fatal("callback waited on its own in-flight pause")
			}
			if !<-done || len(r.named("ai_response_received")) != 1 {
				t.Fatal("outer pause did not finish")
			}
		})
	}
}

func TestHintedPauseMarksStateBeforeFinalization(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.hintOutcome(StatusPaused, "")
	i.onBeforeEnd(func() { i.RecordCall(pauseCall); i.End(EndOptions{Status: StatusError}) })
	i.End(EndOptions{})
	if !i.Paused() || i.Ended() || len(r.named("ai_response_received")) != 1 || lastResponse(t, r)["llm_call_count"] != 1 {
		t.Fatal("finalization ended a hinted pause")
	}
}

type pauseReentrantClient struct {
	*recorder
	reenter func()
}

func (c *pauseReentrantClient) Enqueue(message analytics.Message) error {
	err := c.recorder.Enqueue(message)
	if event, ok := message.(analytics.Track); ok && event.Event == "ai_response_received" && event.Properties["response_status"] == "paused" {
		c.reenter()
	}
	return err
}

func TestPauseDeliveryCarriesLateUsageIntoNextSegment(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			r := setupAI(t)
			client := &pauseReentrantClient{recorder: r}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			call := pauseCall
			call.ResponseID = "first"
			i.RecordCall(call)
			delivering, release := make(chan struct{}), make(chan struct{})
			client.reenter = func() { close(delivering); <-release }
			type result struct {
				snapshot PausedInteraction
				ok       bool
			}
			done := make(chan result, 1)
			go func() { snapshot, ok := i.Pause(PauseOptions{}); done <- result{snapshot, ok} }()
			<-delivering
			call.ResponseID, call.InputTokens, call.CostUSD = "late", 5, Ptr(0.01)
			if !i.RecordCall(call) {
				t.Error("late call rejected")
			}
			i.RecordToolCalls(ToolCall{Name: "late-tool", ID: "late-tool"})
			during, duringOK := i.Pause(PauseOptions{})
			close(release)
			first := <-done
			after, afterOK := i.Pause(PauseOptions{})
			if !first.ok || duringOK || !afterOK || mustJSON(first.snapshot) != mustJSON(after) {
				t.Fatal("snapshots did not include writes accepted during delivery", first, during, after)
			}
			if mustJSON(after.SeenResponseIDs) != `[["openai","first"],["openai","late"]]` {
				t.Fatal(after)
			}
			if restore {
				var stored PausedInteraction
				if err := json.Unmarshal([]byte(mustJSON(after)), &stored); err != nil {
					t.Fatal(err)
				}
				_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			}
			i.Resume()
			if i.Paused() || i.RecordCall(call) {
				t.Fatal("resume lost response ID history")
			}
			i.End(EndOptions{})
			segments := r.named("ai_response_received")
			if len(segments) != 2 {
				t.Fatal(r.names())
			}
			for n, e := range segments {
				wantTokens, wantTools := 12, 0
				if n == 1 {
					wantTokens, wantTools = 7, 1
				}
				if e.Properties["llm_call_count"] != 1 || e.Properties["total_tokens"] != wantTokens || e.Properties["tool_call_count"] != wantTools {
					t.Fatal(e.Properties)
				}
			}
			if segments[1].Properties["total_cost_usd"] != 0.01 {
				t.Fatal(segments[1].Properties)
			}

		})
	}
}

func TestPauseEmissionPreservesEndReentryGuard(t *testing.T) {
	r := setupAI(t)
	client := &pauseReentrantClient{recorder: r}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	var nested PausedInteraction
	var nestedOK bool
	client.reenter = func() {
		nested, nestedOK = i.Pause(PauseOptions{})
		i.End(EndOptions{Status: StatusError})
	}
	i.onBeforeEnd(func() {
		call := pauseCall
		call.ResponseID = "buffered"
		i.RecordCall(call)
	})
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok || nestedOK || len(snapshot.SeenResponseIDs) != 1 || i.Ended() || len(r.named("ai_response_received")) != 1 {
		t.Fatal("emission callback ended the in-flight pause", snapshot, nested)
	}
}

func TestEndDisconnectCallbackRunsWithoutInteractionLock(t *testing.T) {
	for _, status := range []ResponseStatus{StatusPaused, StatusSuccess} {
		t.Run(string(status), func(t *testing.T) {
			r := setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			available := false
			i.disconnected = func() bool {
				done := make(chan struct{})
				go func() { i.Paused(); close(done) }()
				select {
				case <-done:
					available = true
				case <-time.After(time.Second):
				}
				return true
			}
			i.End(EndOptions{Status: status})
			if !available || lastResponse(t, r)["client_disconnected"] != true {
				t.Fatal("disconnect callback ran while the interaction lock was held")
			}
		})
	}
}

func TestConcurrentPauseReportsInProgressUntilCompletedSnapshot(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	finalizing, release, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	type result struct {
		snapshot PausedInteraction
		ok       bool
	}
	first, second := make(chan result, 1), make(chan result, 1)
	i.onBeforeEnd(func() {
		close(finalizing)
		<-release
		call := pauseCall
		call.ResponseID = "buffered"
		i.RecordCall(call)
	})
	go func() { snapshot, ok := i.Pause(PauseOptions{}); first <- result{snapshot, ok} }()
	<-finalizing
	go func() {
		close(secondStarted)
		snapshot, ok := i.Pause(PauseOptions{})
		second <- result{snapshot, ok}
	}()
	<-secondStarted
	var b result
	select {
	case b = <-second:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("concurrent pause waited on unfinished finalization")
	}
	close(release)
	a := <-first
	if b.ok {
		t.Fatal("concurrent pause exposed an unfinished snapshot")
	}
	b.snapshot, b.ok = i.Pause(PauseOptions{})
	if !a.ok || !b.ok || mustJSON(a.snapshot) != mustJSON(b.snapshot) || mustJSON(b.snapshot.SeenResponseIDs) != `[["openai","buffered"]]` {
		t.Fatal(a, b)
	}
	if len(r.named("ai_response_received")) != 1 {
		t.Fatal(r.names())
	}
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &b.snapshot})
	call := pauseCall
	call.ResponseID = "buffered"
	if resumed.RecordCall(call) {
		t.Fatal("restored unfinished snapshot counted buffered response twice")
	}
}

func TestFinalizationHooksRearmAcrossPauses(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for range 2 {
		i.onBeforeEnd(func() { i.RecordCall(pauseCall) })
		i.Pause(PauseOptions{})
		i.Resume()
	}
	i.End(EndOptions{})
	for n, e := range r.named("ai_response_received") {
		count := 1
		if n == 2 {
			count = 0
		}
		if e.Properties["llm_call_count"] != count || e.Properties["total_tokens"] != 12*count {
			t.Fatal(e.Properties)
		}
	}
}

func TestSnapshotOrderingToleratesInvalidUnicode(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for _, id := range []string{"\ue000", "\U00010000", "\xed\xa0\x80", "a"} {
		call := pauseCall
		call.ResponseID = id
		i.RecordCall(call)
	}
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok || len(snapshot.SeenResponseIDs) != 4 {
		t.Fatal(snapshot)
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var stored PausedInteraction
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	if _, ok := resumed.Pause(PauseOptions{}); !ok {
		t.Fatal("invalid Unicode prevented restoration")
	}
}

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

// TryLock detects the original bug without stranding the test process on a callback deadlock.
func TestAllCallbackTypesReleaseLockAndAllowLifecycleReentry(t *testing.T) {
	for _, source := range []string{"onError-call", "onError-tool", "redactor", "finalizer", "when", "properties", "delivery", "disconnect", "marshal"} {
		t.Run(source, func(t *testing.T) {
			var i *Interaction
			entered := false
			var nestedOK, accepted bool
			reenter := func() {
				if entered {
					return
				}
				entered = true
				if !i.mu.TryLock() {
					t.Error("application callback held the interaction mutex")
					return
				}
				i.mu.Unlock()
				_, nestedOK = i.Pause(PauseOptions{})
				i.End(EndOptions{})
				call := pauseCall
				call.ResponseID = "callback"
				accepted = i.RecordCall(call)
			}
			r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithToolBreakdown(true),
				WithLimits(Limits{MaxCallsPerInteraction: 1}),
				WithOnError(func(error, ErrorInfo) {
					if source == "onError-call" || source == "onError-tool" {
						reenter()
					}
				}),
				WithRedactHook(func(text string) (string, error) {
					if source == "redactor" {
						reenter()
					}
					return text, nil
				}),
				WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_done",
					When: func(map[string]any, any) bool {
						if source == "when" {
							reenter()
						}
						return true
					},
					Properties: func(map[string]any, any) map[string]any {
						if source == "properties" {
							reenter()
						}
						if source == "marshal" {
							return map[string]any{"value": reentrantJSON{reenter}}
						}
						return nil
					},
				}}}))
			client := &pauseReentrantClient{recorder: r, reenter: func() {
				if source == "delivery" {
					reenter()
				}
			}}
			_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Analytics: client})
			call := pauseCall
			call.ResponseID = "initial"
			i.RecordCall(call)
			i.RecordToolCalls(ToolCall{ID: "tool", Name: "lookup"})
			i.onBeforeEnd(func() {
				if source == "finalizer" {
					reenter()
				}
				if source == "onError-call" {
					call.ResponseID = "overflow"
					i.RecordCall(call)
				}
				if source == "onError-tool" {
					i.RecordToolCalls(ToolCall{ID: "overflow", Name: "other"})
				}
			})
			if source == "disconnect" {
				i.disconnected = func() bool { reenter(); return false }
			}
			done := make(chan bool, 1)
			go func() { _, ok := i.Pause(PauseOptions{ResponseText: "approval"}); done <- ok }()
			select {
			case ok := <-done:
				if !ok {
					t.Fatal("outer pause failed")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("callback deadlocked")
			}
			if !entered || nestedOK || !accepted {
				t.Fatal("callback lifecycle results", entered, nestedOK, accepted)
			}
			i.Resume()
			i.End(EndOptions{})
			segments := r.named("ai_response_received")
			calls := 2
			if source == "onError-call" {
				calls = 3
			}
			if len(segments) != 2 || len(r.named("ai_llm_response_received")) != calls || len(r.named("lookup_done")) != 1 {
				t.Fatal("double or missing emission", r.names())
			}
			total, tokens := 0, 0
			for _, e := range segments {
				total += e.Properties["llm_call_count"].(int)
				tokens += e.Properties["total_tokens"].(int)
			}
			if total != calls || tokens != calls*12 {
				t.Fatal("lost usage", total, tokens)
			}
		})
	}
}

type reentrantJSON struct{ callback func() }

func (v reentrantJSON) MarshalJSON() ([]byte, error) { v.callback(); return []byte(`"value"`), nil }

func TestBackgroundCallbackTypesAllowLifecycleReentry(t *testing.T) {
	for _, source := range []string{"classifier", "evaluator"} {
		t.Run(source, func(t *testing.T) {
			var i *Interaction
			result := make(chan bool, 1)
			reenter := func() {
				if !i.mu.TryLock() {
					result <- false
					return
				}
				i.mu.Unlock()
				_, ok := i.Pause(PauseOptions{})
				i.End(EndOptions{})
				accepted := i.RecordCall(pauseCall)
				result <- !ok && !accepted
			}
			var option ConfigOption
			if source == "classifier" {
				option = WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) {
					reenter()
					return ClassifyResult{Intent: "other", Outcome: "resolved"}, nil
				}})
			} else {
				option = WithEvaluator(func(context.Context, EvaluationTurn) ([]Score, error) {
					reenter()
					return []Score{{EvaluationName: "quality", Evaluator: EvaluatorCode, ScoreValue: Ptr(1.0)}}, nil
				})
			}
			r := setupAI(t, option)
			_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
			i.RecordCall(pauseCall)
			i.End(EndOptions{ResponseText: "done"})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if !i.waitBackground(ctx) {
				t.Fatal("background callback deadlocked")
			}
			if !<-result {
				t.Fatal("background reentry failed")
			}
			event := "ai_interaction_classified"
			if source == "evaluator" {
				event = "ai_interaction_scored"
			}
			if len(r.named("ai_response_received")) != 1 || len(r.named(event)) != 1 || lastResponse(t, r)["total_tokens"] != 12 {
				t.Fatal("double emission or lost usage", r.names())
			}
		})
	}
}

func TestPauseDeliveryReentryRemainsInProgress(t *testing.T) {
	r := setupAI(t)
	client := &pauseReentrantClient{recorder: r}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Analytics: client})
	i.RecordCall(pauseCall)
	results := make(chan bool, 2)
	client.reenter = func() {
		// A delivery callback can delegate to another goroutine and wait for it.
		go func() { _, ok := i.Pause(PauseOptions{}); results <- ok }()
		select {
		case ok := <-results:
			if ok {
				t.Error("delivery exposed a completed pause")
			}
		case <-time.After(time.Second):
			t.Error("delivery callback blocked waiting on Pause")
		}
		i.End(EndOptions{})
		call := pauseCall
		call.ResponseID = "delivery"
		if !i.RecordCall(call) {
			t.Error("late usage rejected")
		}
		_, ok := i.Pause(PauseOptions{})
		if ok {
			t.Error("nested pause after late usage returned ok")
		}
	}
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok || len(snapshot.PendingCalls) != 1 {
		t.Fatal("outer pause lost late usage", snapshot, ok)
	}
	after, afterOK := i.Pause(PauseOptions{})
	if !afterOK || mustJSON(snapshot) != mustJSON(after) {
		t.Fatal("completed snapshot changed")
	}
	i.Resume()
	i.End(EndOptions{})
	segments := r.named("ai_response_received")
	if len(segments) != 2 || segments[0].Properties["llm_call_count"] != 1 || segments[1].Properties["llm_call_count"] != 1 {
		t.Fatal("lost or doubled usage", r.names())
	}
}
