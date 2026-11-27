package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/dlclark/regexp2"
	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func (s *seenErrors) count(info ErrorInfo) int {
	n := 0
	for _, i := range s.all() {
		if i == info {
			n++
		}
	}
	return n
}

func TestWithLimitsValidates(t *testing.T) {
	setupAI(t)
	if err := Configure(WithLimits(Limits{MaxOpenInteractions: -1})); err == nil {
		t.Fatal("negative limit accepted")
	}
	if err := Configure(WithLimits(Limits{MaxMapEntries: 5})); err != nil {
		t.Fatal(err)
	}
	if got := cfg().limits; got.MaxMapEntries != 5 || got.MaxOpenInteractions != 10_000 || got.InteractionTTL != time.Hour || got.MaxCallsPerInteraction != 1_000 {
		t.Fatalf("limits = %+v; zero fields keep their defaults", got)
	}
}

func TestMaxOpenInteractionsRefusesNew(t *testing.T) {
	seen := &seenErrors{}
	r := setupAI(t, WithOnError(seen.hook), WithLimits(Limits{MaxOpenInteractions: 3}))
	var open []*Interaction
	for range 3 {
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
		open = append(open, i)
	}
	want := errors.New("user error")
	var refused *Interaction
	err := RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(ctx context.Context, i *Interaction) error {
		refused = i
		if c, _ := forCall(ctx, "test", nil); c != nil {
			t.Error("a call inside a refused interaction was tracked")
		}
		i.RecordCall(LLMCall{Provider: "openai", Model: "m"})
		return want
	})
	if err != want {
		t.Fatalf("err = %v; the caller's code must run as usual", err)
	}
	if openCount() != 3 || !refused.Ended() || len(r.all()) != 0 {
		t.Fatalf("open = %d, events %v", openCount(), r.names())
	}
	if seen.count(ErrorInfo{Kind: KindLimitReached, Where: "MaxOpenInteractions"}) != 1 {
		t.Fatalf("hook saw %+v", seen.all())
	}
	for _, i := range open {
		i.End(EndOptions{})
	}
	if openCount() != 0 {
		t.Fatalf("open = %d after the ends", openCount())
	}
	answer(t, InteractionOptions{}, "") // room again
	if len(r.named("ai_response_received")) != 4 {
		t.Fatalf("events %v", r.names())
	}
}

func TestInteractionTTLEndsStaleOnes(t *testing.T) {
	r := setupAI(t, WithLimits(Limits{InteractionTTL: 20 * time.Millisecond}))
	_, stale := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	stale.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 3})
	time.Sleep(40 * time.Millisecond)
	answer(t, InteractionOptions{}, "") // its start sweeps
	if !stale.Ended() || openCount() != 0 {
		t.Fatalf("stale interaction still open (%d open)", openCount())
	}
	var p map[string]any
	for _, e := range r.named("ai_response_received") {
		if e.Properties["interaction_id"] == stale.ID() {
			p = e.Properties
		}
	}
	if p["response_status"] != "incomplete" || p["end_reason"] != "ttl_expired" || p["total_input_tokens"] != 3 {
		t.Fatalf("ttl end: %v", p)
	}
}

func TestEndReasonIsReserved(t *testing.T) {
	r := setupAI(t)
	run(t, InteractionOptions{NoPromptEvent: true}, func(_ context.Context, i *Interaction) {
		i.End(EndOptions{ExtraProperties: map[string]any{"end_reason": "shutdown"}})
	})
	if _, ok := r.response(t)["end_reason"]; ok {
		t.Fatal("end_reason is the SDK's")
	}
}

func TestMaxCallsPerInteractionKeepsTotals(t *testing.T) {
	seen := &seenErrors{}
	r := setupAI(t, WithOnError(seen.hook), WithToolBreakdown(true), WithLimits(Limits{MaxCallsPerInteraction: 2}))
	var in *Interaction
	run(t, InteractionOptions{NoPromptEvent: true}, func(_ context.Context, i *Interaction) {
		in = i
		for n := range 3 {
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1, OutputTokens: 1, CostUSD: Ptr(2.0), ResponseID: string(rune('a' + n))})
		}
		i.RecordCall(LLMCall{Provider: "openai", Model: "other", ErrorType: "rate_limit"})
		for n := range 3 {
			// They ran: one with an ID and no outcome past the cap is kept, in case a pause waits on it.
			i.RecordToolCalls(ToolCall{ID: string(rune('a' + n)), Name: "search", Error: Ptr(false)})
		}
		i.RecordToolCallCount(2)
	})
	p := r.response(t)
	if p["llm_call_count"] != 4 || p["total_input_tokens"] != 3 || p["total_cost_usd"] != 6.0 || p["tool_call_count"] != 5 {
		t.Fatalf("totals: %v", p)
	}
	if p["response_status"] != "error" || p["error_type"] != "rate_limit" {
		t.Fatalf("the last call past the cap still decides the status: %v", p)
	}
	// Calls past the cap are folded per breakdown group, not into a kept call, as in Node and Python.
	b := p["usage_breakdown"].([]map[string]any)
	if len(b) != 2 || b[0]["model"] != "m" || b[0]["call_count"] != 3 || b[0]["cost_usd"] != 6.0 || b[1]["model"] != "other" || b[1]["call_count"] != 1 || b[1]["error_count"] != 1 {
		t.Fatalf("usage_breakdown: %v", b)
	}
	tb := p["tool_breakdown"].([]map[string]any)
	if len(tb) != 2 || tb[0]["tool_name"] != "search" || tb[0]["call_count"] != 3 || tb[1]["tool_name"] != "unknown" || tb[1]["call_count"] != 2 {
		t.Fatalf("tool_breakdown: %v", tb)
	}
	if len(in.calls) != 2 || len(in.tools) != 2 {
		t.Fatalf("kept %d calls, %d tools; want the cap", len(in.calls), len(in.tools))
	}
	if in.calls[0].callCount != 1 || in.calls[1].callCount != 1 {
		t.Fatal("a kept call absorbed a call past the cap")
	}
	if seen.count(ErrorInfo{Kind: KindLimitReached, Where: "MaxCallsPerInteraction"}) != 1 {
		t.Fatalf("hook saw %+v", seen.all())
	}
}

func TestUnpricedCallPastTheCapDropsTheTotal(t *testing.T) {
	r := setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: 1}))
	run(t, InteractionOptions{NoPromptEvent: true}, func(_ context.Context, i *Interaction) {
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", CostUSD: Ptr(1.0)})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", UsageUnknown: true})
	})
	if _, ok := r.response(t)["total_cost_usd"]; ok {
		t.Fatal("total_cost_usd sent though a call was unpriced")
	}
}

func TestRememberedClientsCapped(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook), WithLimits(Limits{MaxMapEntries: 2}))
	sinks := []*sink{{}, {}, {}}
	for _, s := range sinks {
		emitAI(s, "ai_user_action", "u", "", map[string]any{}, nil, "", time.Now())
	}
	got := rememberedClients()
	if len(got) != 2 || got[0] != analytics.Client(sinks[1]) || got[1] != analytics.Client(sinks[2]) {
		t.Fatalf("remembered %v; want the 2 newest", got)
	}
	if seen.count(ErrorInfo{Kind: KindLimitReached, Where: "MaxMapEntries (clients)"}) != 1 {
		t.Fatalf("hook saw %+v", seen.all())
	}
}

func TestMCPExpiryEvictionStopsItsTimer(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxMapEntries: 1}))
	st := newServerState()
	fired := make(chan struct{}, 1)
	st.expiries.set("a", time.AfterFunc(30*time.Millisecond, func() { fired <- struct{}{} }))
	st.expiries.set("b", time.AfterFunc(time.Hour, func() {}))
	select {
	case <-fired:
		t.Fatal("an evicted expiry still fired")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestMaxMapEntriesReachesIntegrations(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxMapEntries: 7}))
	if hook.MaxMapEntries() != 7 || newServerState().scopes.max != 7 {
		t.Fatal("integrations do not see MaxMapEntries")
	}
}

// blockingClient is a full analytics-go queue: Enqueue waits until released.
type blockingClient struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingClient) Enqueue(analytics.Message) error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}

func (b *blockingClient) Close() error { return nil }

func TestEnqueueRunsOutsideTheInteractionLock(t *testing.T) {
	setupAI(t)
	b := &blockingClient{entered: make(chan struct{}, 4), release: make(chan struct{})}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Analytics: b})
	ended := make(chan struct{})
	go func() {
		i.End(EndOptions{})
		close(ended)
	}()
	<-b.entered
	done := make(chan struct{})
	go func() {
		i.SetIntent("x")
		_ = i.Ended()
		i.RecordCall(LLMCall{Provider: "openai", Model: "m"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked Enqueue held the interaction lock")
	}
	close(b.release)
	<-ended
}

func TestRedactionTimeoutSendsPlaceholder(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook))
	// Catastrophic backtracking: without the timeout this runs for minutes.
	slow := regexp2.MustCompile(`(a+)+$`, regexp2.None)
	slow.MatchTimeout = time.Millisecond
	rules := []redactRule{{name: "slow", token: "secret"}}
	if got := maskWith(rules, []*regexp2.Regexp{slow}, strings.Repeat("a", 40)+"!"); got != redactionTimedOut {
		t.Fatalf("masked = %q; want the placeholder, never the unmasked text", got)
	}
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindRedactionTimeout, Where: "redaction pattern slow"}) {
		t.Fatalf("hook saw %+v", got)
	}
	for _, re := range compiledRules {
		if re.MatchTimeout != redactMatchTimeout*raceSlowdown {
			t.Fatal("a built-in pattern has no timeout")
		}
	}
}

func TestRedactionTimeoutInFoldedViewSendsPlaceholder(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook))
	slow := regexp2.MustCompile(`(a+)+$`, regexp2.None)
	slow.MatchTimeout = time.Millisecond
	rules := []redactRule{{name: "slow", token: "secret"}}
	// Short runs keep the text's own view fast; the folded view drops the zero-width spaces and backtracks.
	if got := maskWith(rules, []*regexp2.Regexp{slow}, strings.Repeat("aaaa\u200b", 10)+"!"); got != redactionTimedOut {
		t.Fatalf("masked = %q; want the placeholder, never the unmasked text", got)
	}
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindRedactionTimeout, Where: "redaction pattern slow"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestTimedOutTextSkipsTheRedactHook(t *testing.T) {
	called := false
	setupAI(t, WithRedactHook(func(s string) (string, error) { called = true; return s, nil }))
	o := &options{Options: Options{Redact: func(s string) (string, error) { called = true; return s, nil }}}
	if out, ok := o.mask(redactionTimedOut); !ok || out != redactionTimedOut || called {
		t.Fatalf("mask = %q, %v, hook called %v", out, ok, called)
	}
}

func pausedSlotsFixture(t *testing.T, o ...ConfigOption) (*Interaction, PausedInteraction) {
	t.Helper()
	setupAI(t, append(o, WithLimits(Limits{MaxOpenInteractions: 1}))...)
	_, a := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	snap, ok := a.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause failed")
	}
	return a, snap
}

func TestPausedRunKeepsItsSlotAndBlocksNewInteractions(t *testing.T) {
	a, _ := pausedSlotsFixture(t)
	_, b := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if !b.Ended() {
		t.Fatal("B admitted while A is paused at limit 1")
	}
	a.End(EndOptions{})
	_, c := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if c.Ended() {
		t.Fatal("C refused after A ended")
	}
	c.End(EndOptions{})
}

func TestResumeInProcessSucceedsAtLimit(t *testing.T) {
	a, _ := pausedSlotsFixture(t)
	_, b := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if !b.Ended() {
		t.Fatal("B admitted")
	}
	a.Resume()
	if a.Paused() || openCount() != 1 {
		t.Fatalf("resume failed: paused=%v open=%d", a.Paused(), openCount())
	}
	a.End(EndOptions{})
}

func TestRestoreOfCountedPausedRunSucceedsAtLimit(t *testing.T) {
	a, snap := pausedSlotsFixture(t)
	_, b := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if !b.Ended() {
		t.Fatal("B admitted")
	}
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snap})
	if restored.Ended() || restored.ID() != a.ID() || openCount() != 1 {
		t.Fatalf("counted restore refused (ended=%v open=%d)", restored.Ended(), openCount())
	}
	restored.End(EndOptions{})
	_, c := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if c.Ended() {
		t.Fatal("C refused after the restored run ended")
	}
	c.End(EndOptions{})
}

func TestRestoreOfForeignPausedRunGoesThroughAdmission(t *testing.T) {
	a, snap := pausedSlotsFixture(t)
	foreign := snap
	foreign.InteractionID = "other-worker-run"
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &foreign})
	if !restored.Ended() {
		t.Fatal("uncounted restore bypassed the limit")
	}
	a.End(EndOptions{})
	_, ok := StartInteraction(context.Background(), InteractionOptions{Resume: &foreign})
	if ok.Ended() {
		t.Fatal("uncounted restore refused with room")
	}
	ok.End(EndOptions{})
}

func TestPausedSlotsCountOwnersWithSharedID(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
	start := func(user string) *Interaction {
		_, i := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: user, NoPromptEvent: true})
		return i
	}
	a, b := start("a"), start("b")
	defer a.End(EndOptions{})
	defer b.End(EndOptions{})
	a.Pause(PauseOptions{})
	b.Pause(PauseOptions{})
	if c := start("c"); !c.Ended() {
		c.End(EndOptions{})
		t.Fatal("shared-ID paused owners must each hold a slot")
	}
	a.Resume()
	if c := start("c"); !c.Ended() {
		c.End(EndOptions{})
		t.Fatal("resuming A freed B's slot")
	}
	a.End(EndOptions{})
	c := start("c")
	defer c.End(EndOptions{})
	if c.Ended() {
		t.Fatal("ending A did not free exactly its slot")
	}
	if d := start("d"); !d.Ended() {
		d.End(EndOptions{})
		t.Fatal("ending A freed paused B's slot")
	}
}

func TestRestoreSharedIDCannotTakeForeignPausedSlot(t *testing.T) {
	for _, field := range []string{"conversation", "user", "anonymous"} {
		t.Run(field, func(t *testing.T) {
			a, snapshot := pausedSlotsFixture(t)
			defer a.End(EndOptions{})
			switch field {
			case "conversation":
				snapshot.ConversationID = "another-conversation"
			case "user":
				snapshot.UserID = "another-user"
			case "anonymous":
				snapshot.AnonymousID = "another-anonymous-user"
			}
			_, foreign := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
			defer foreign.End(EndOptions{})
			if !foreign.Ended() {
				t.Fatal("shared ID let a foreign restore take A's slot")
			}
		})
	}
}

func TestRestoreDuplicateIdentityPreservesPausedSlots(t *testing.T) {
	for _, limit := range []int{2, 3} {
		for _, restoreIndex := range []int{0, 1} {
			t.Run(fmt.Sprintf("limit=%d/restore=%d", limit, restoreIndex), func(t *testing.T) {
				setupAI(t, WithLimits(Limits{MaxOpenInteractions: limit}))
				var originals [2]*Interaction
				var snapshots [2]PausedInteraction
				var slots [2]pausedSlot
				for n := range originals {
					_, originals[n] = StartInteraction(context.Background(), InteractionOptions{
						InteractionID: "shared", UserID: "user", AnonymousID: "anonymous", ConversationID: "conversation", Classify: Ptr(false),
					})
					defer originals[n].End(EndOptions{})
					var ok bool
					snapshots[n], ok = originals[n].Pause(PauseOptions{})
					if !ok {
						t.Fatal("pause failed")
					}
					slots[n] = slotFor(originals[n])
				}
				// A snapshot without a pause token (an older SDK version) matches no slot, even
				// by identity.
				snapshots[restoreIndex].PauseToken = ""
				_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshots[restoreIndex], Classify: Ptr(false)})
				defer restored.End(EndOptions{})
				if restored.Ended() != (limit == 2) {
					t.Errorf("ambiguous restore admission: ended=%v, limit=%d", restored.Ended(), limit)
				}
				restored.End(EndOptions{})
				open.mu.Lock()
				defer open.mu.Unlock()
				if len(open.paused) != 2 || open.list.Len() != 0 {
					t.Errorf("restore changed counted originals: paused=%d, open=%d", len(open.paused), open.list.Len())
				}
				for n, original := range originals {
					if got, ok := open.paused[weak.Make(original)]; !ok || got != slots[n] {
						t.Errorf("original %d lost or changed its paused slot: got=%+v, present=%v", n, got, ok)
					}
				}
			})
		}
	}
}

func TestPausedSharedIDOwnersRemainWeakAndRestoreAfterGC(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
	var snapshots [2]PausedInteraction
	var refs [2]weak.Pointer[Interaction]
	for n := range snapshots {
		snapshots[n], refs[n] = func() (PausedInteraction, weak.Pointer[Interaction]) {
			_, i := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: fmt.Sprintf("tenant-%d", n), Classify: Ptr(false)})
			snapshot, ok := i.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause failed")
			}
			return snapshot, weak.Make(i)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for refs[0].Value() != nil || refs[1].Value() != nil {
		if time.Now().After(deadline) {
			t.Fatal("paused registry retained an owner")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	for n, snapshot := range snapshots {
		var stored PausedInteraction
		if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
			t.Fatal(err)
		}
		_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		defer restored.End(EndOptions{})
		if restored.Ended() || restored.userID != fmt.Sprintf("tenant-%d", n) {
			t.Fatal("restore did not take its own collected owner's slot")
		}
	}
	_, extra := StartInteraction(context.Background(), InteractionOptions{UserID: "extra"})
	defer extra.End(EndOptions{})
	if !extra.Ended() {
		t.Fatal("restores changed the total number of counted slots")
	}
}

// A masking timeout gives the placeholder; agent-written MCP text withholds it (spec/fixtures/mcp-agent-text-v0.1.json).
func TestAgentTextWithheldOnMaskingTimeout(t *testing.T) {
	setupAI(t, WithOnError(func(error, ErrorInfo) {}))
	slow := regexp2.MustCompile(`(a+)+$`, regexp2.None)
	slow.MatchTimeout = time.Millisecond
	saved := compiledRules[0]
	compiledRules[0] = slow
	t.Cleanup(func() { compiledRules[0] = saved })
	o := &options{}
	if got := o.maskedText(strings.Repeat("a", 40)+"!", maxTextLength); got != "" {
		t.Fatalf("maskedText = %q, want it withheld", got)
	}
}

func TestRestoreWithPauseTokenTakesOnlyItsOwnPausedSlot(t *testing.T) {
	for _, restoreIndex := range []int{0, 1} {
		t.Run(fmt.Sprintf("restore=%d", restoreIndex), func(t *testing.T) {
			setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
			var originals [2]*Interaction
			var snapshots [2]PausedInteraction
			for n := range originals {
				_, originals[n] = StartInteraction(context.Background(), InteractionOptions{
					InteractionID: "shared", UserID: "user", AnonymousID: "anonymous", ConversationID: "conversation", Classify: Ptr(false),
				})
				defer originals[n].End(EndOptions{})
				var ok bool
				snapshots[n], ok = originals[n].Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause failed")
				}
			}
			if snapshots[0].PauseToken == "" || snapshots[0].PauseToken == snapshots[1].PauseToken {
				t.Fatalf("pause tokens must be set and distinct: %q, %q", snapshots[0].PauseToken, snapshots[1].PauseToken)
			}
			_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshots[restoreIndex], Classify: Ptr(false)})
			defer restored.End(EndOptions{})
			if restored.Ended() {
				t.Fatal("a restore holding its pause's token must take that pause's slot")
			}
			open.mu.Lock()
			defer open.mu.Unlock()
			if _, ok := open.paused[weak.Make(originals[restoreIndex])]; ok {
				t.Error("the restored pause's slot was not transferred")
			}
			if _, ok := open.paused[weak.Make(originals[1-restoreIndex])]; !ok {
				t.Error("the other pause lost its slot")
			}
		})
	}
}

// countedSlots is what admission counts: open runs plus paused runs still in the registry.
func countedSlots() (counted, paused int) {
	open.mu.Lock()
	defer open.mu.Unlock()
	return open.list.Len() + len(open.paused), len(open.paused)
}

func waitPausedRegistry(t *testing.T, want func(paused int) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		if _, paused := countedSlots(); want(paused) {
			return
		}
		if time.Now().After(deadline) {
			_, paused := countedSlots()
			t.Fatalf("paused registry holds %d entries", paused)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCollectedPausesReleaseSlotsAndKeepRegistryBounded(t *testing.T) {
	const limit = 10_000
	setupAI(t)
	maxSeen := 0
	for n := range 2 * limit {
		func() {
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Classify: Ptr(false)})
			if i.Ended() {
				t.Fatalf("pause %d: interaction not tracked", n)
			}
			if _, ok := i.Pause(PauseOptions{}); !ok {
				t.Fatalf("pause %d failed", n)
			}
		}()
		if n%500 == 499 {
			runtime.GC()
		}
		if _, paused := countedSlots(); paused > maxSeen {
			maxSeen = paused
		}
	}
	if maxSeen > limit {
		t.Fatalf("paused registry grew to %d, over the cap of %d", maxSeen, limit)
	}
	waitPausedRegistry(t, func(paused int) bool { return paused == 0 })
	if counted, _ := countedSlots(); counted != 0 {
		t.Fatalf("admission still counts %d collected pauses", counted)
	}
	_, next := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	defer next.End(EndOptions{})
	if next.Ended() {
		t.Fatal("new interaction untracked after the collected pauses")
	}
}

func TestCollectedPauseRestoreUsesNormalAdmissionAndNeverReachesAnotherTenant(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
	pause := func(user string) (PausedInteraction, *Interaction) {
		_, i := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: user, NoPromptEvent: true, Classify: Ptr(false)})
		snap, ok := i.Pause(PauseOptions{})
		if !ok {
			t.Fatal("pause failed")
		}
		return snap, i
	}
	snapA, ref := func() (PausedInteraction, weak.Pointer[Interaction]) {
		s, i := pause("tenant-a")
		return s, weak.Make(i)
	}()
	requireCollected(t, ref)
	_, b := pause("tenant-b") // Live: still counts.
	waitPausedRegistry(t, func(paused int) bool { return paused == 1 })

	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapA, Classify: Ptr(false)})
	if restored.Ended() || restored.userID != "tenant-a" {
		t.Fatal("collected pause's restore was not admitted normally")
	}
	if !b.Paused() {
		t.Fatal("restore of a collected pause reached another tenant's paused run")
	}
	if _, ok := open.paused[weak.Make(b)]; !ok {
		t.Fatal("restore took over another tenant's paused slot")
	}
	if counted, _ := countedSlots(); counted != 2 {
		t.Fatalf("counted slots = %d, want 2 (live pause + normal admission)", counted)
	}
	_, extra := StartInteraction(context.Background(), InteractionOptions{UserID: "extra"})
	if !extra.Ended() {
		t.Fatal("cap not enforced after the normal admission")
	}
	restored.End(EndOptions{})
	b.End(EndOptions{})
}

func TestLivePausedRunKeepsCountingAmongCollectedOnes(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxOpenInteractions: 3}))
	for range 5 {
		func() {
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "gone", NoPromptEvent: true, Classify: Ptr(false)})
			i.Pause(PauseOptions{})
		}()
		runtime.GC()
	}
	_, live := StartInteraction(context.Background(), InteractionOptions{UserID: "live", NoPromptEvent: true, Classify: Ptr(false)})
	snap, ok := live.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause failed")
	}
	waitPausedRegistry(t, func(paused int) bool { return paused == 1 })
	runtime.KeepAlive(live)
	if counted, _ := countedSlots(); counted != 1 {
		t.Fatalf("counted = %d, want only the live pause", counted)
	}
	var others []*Interaction
	for range 2 {
		_, o := StartInteraction(context.Background(), InteractionOptions{UserID: "other", NoPromptEvent: true})
		others = append(others, o)
	}
	_, over := StartInteraction(context.Background(), InteractionOptions{UserID: "over", NoPromptEvent: true})
	if !over.Ended() {
		t.Fatal("live paused run did not count toward the cap")
	}
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snap, Classify: Ptr(false)})
	if restored.Ended() || restored.userID != "live" {
		t.Fatal("live paused run's own restore refused at the cap")
	}
	restored.End(EndOptions{})
	for _, o := range others {
		o.End(EndOptions{})
	}
	runtime.KeepAlive(live)
}

func TestSDK7017RetirementMapEvictionReportsLimit(t *testing.T) {
	seen := &seenErrors{}
	callback := false
	setupAI(t, WithLimits(Limits{MaxMapEntries: 2}), WithOnError(func(err error, info ErrorInfo) {
		seen.hook(err, info)
		if info.Where == "MaxMapEntries (retired interactions)" {
			callback = true
			// Reporting must happen outside the registry lock.
			if openCount() != 0 {
				t.Error("retirement report preceded unregister")
			}
		}
	}))
	end := func(id string) {
		_, i := StartInteraction(context.Background(), InteractionOptions{InteractionID: id, NoPromptEvent: true, Classify: Ptr(false)})
		i.End(EndOptions{})
	}
	end("a")
	end("b")
	end("a") // Updating an existing ID does not evict.
	if len(seen.all()) != 0 {
		t.Fatalf("reported before eviction: %+v", seen.all())
	}
	end("c")
	end("d") // The same kind is rate-limited across repeated evictions and other maps.
	for _, s := range []*sink{{}, {}, {}} {
		rememberClient(s)
	}
	if !callback || seen.count(ErrorInfo{Kind: KindLimitReached, Where: "MaxMapEntries (retired interactions)"}) != 1 || len(seen.all()) != 1 {
		t.Fatalf("hook saw %+v; want one limit_reached for retirement-map eviction", seen.all())
	}
	open.mu.Lock()
	defer open.mu.Unlock()
	if len(open.retired) != 2 || open.retired["c"] == nil || open.retired["d"] == nil || open.retiredFloor == 0 {
		t.Fatal("retirement map did not retain the newest IDs and advance its floor")
	}
}
