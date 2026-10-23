package rudderstackai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
			i.RecordToolCalls(ToolCall{ID: string(rune('a' + n)), Name: "search"})
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

func TestTimedOutTextSkipsTheRedactHook(t *testing.T) {
	called := false
	setupAI(t, WithRedactHook(func(s string) (string, error) { called = true; return s, nil }))
	o := &options{Options: Options{Redact: func(s string) (string, error) { called = true; return s, nil }}}
	if out, ok := o.mask(redactionTimedOut); !ok || out != redactionTimedOut || called {
		t.Fatalf("mask = %q, %v, hook called %v", out, ok, called)
	}
}
