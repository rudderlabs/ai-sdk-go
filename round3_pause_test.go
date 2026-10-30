package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func round3Resume(t *testing.T, i *Interaction, s PausedInteraction, restore bool) *Interaction {
	t.Helper()
	if restore {
		var stored PausedInteraction
		if err := json.Unmarshal([]byte(mustJSON(s)), &stored); err != nil {
			t.Fatal(err)
		}
		_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	} else {
		i.Resume()
	}
	return i
}

func TestRound3ReportSequence(t *testing.T) {
	for _, cap := range []int{1, 100} {
		for _, restore := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", cap, restore), func(t *testing.T) {
				r := setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: cap}))
				client := &pauseReentrantClient{recorder: r}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				i.RecordCall(LLMCall{Provider: "openai", Model: "closing", InputTokens: 1})
				client.reenter = func() {
					i.RecordCall(LLMCall{Provider: "openai", Model: "first", CallCount: 3})
					i.RecordCall(LLMCall{Provider: "openai", Model: "latest", CallCount: 3})
					i.RecordCall(LLMCall{Provider: "openai", Model: "failed", ErrorType: "api_error"})
				}
				s, ok := i.Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause")
				}
				if s.ReportCount == nil || *s.ReportCount != 4 || s.SuccessfulCallSequence == nil || *s.SuccessfulCallSequence != 3 {
					t.Fatal("missing sequence counters", s)
				}
				i = round3Resume(t, i, s, restore)
				if i.successfulCallSequence != 3 {
					t.Fatalf("success sequence %d, want 3", i.successfulCallSequence)
				}
				if i.callCount != 4 {
					t.Fatalf("reports %d, want 4", i.callCount)
				}
				i.hintOutcome(StatusIncomplete, "max_turns")
				i.End(EndOptions{})
				p := lastResponse(t, r)
				if p["llm_call_count"] != 7 || p["primary_model"] != "latest" || p["response_status"] != "incomplete" {
					t.Fatal(p)
				}
			})
		}
	}
}

func TestRound3PendingCompaction(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			r := setupAI(t)
			client := &pauseReentrantClient{recorder: r}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			client.reenter = func() { i.RecordCompaction() }
			s, _ := i.Pause(PauseOptions{})
			if s.PendingCompactions == nil || *s.PendingCompactions != 1 {
				t.Fatal("missing pending compactions", s)
			}
			i = round3Resume(t, i, s, restore)
			i.End(EndOptions{})
			if lastResponse(t, r)["compaction_count"] != 1 {
				t.Fatal(lastResponse(t, r))
			}
		})
	}
}

func TestRound3PendingOverflowAndRules(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, suppressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%t", restore, suppressed), func(t *testing.T) {
				evaluations := 0
				r := setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: 1}), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
					"tool":     {Event: "tool_done", When: func(args map[string]any, result any) bool { evaluations++; return !suppressed }, Properties: func(args map[string]any, result any) map[string]any { return map[string]any{"kind": args["kind"]} }},
					"overflow": {Event: "overflow_done"},
				}}))
				client := &pauseReentrantClient{recorder: r}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				client.reenter = func() {
					i.RecordToolCalls(ToolCall{Name: "tool", Args: map[string]any{"kind": "list", "secret": "raw-secret"}, Result: "raw-result", Error: Ptr(false)}, ToolCall{Name: "overflow"})
				}
				s, _ := i.Pause(PauseOptions{})
				if evaluations != 1 || len(r.named("tool_done")) != 0 || !strings.Contains(mustJSON(s), "rule_outcome") || !strings.Contains(mustJSON(s), "overflow") || strings.Contains(mustJSON(s), "raw-secret") || strings.Contains(mustJSON(s), "raw-result") {
					t.Fatal("rules not frozen at pause", evaluations, mustJSON(s), r.names())
				}
				i = round3Resume(t, i, s, restore)
				i.End(EndOptions{})
				want := 1
				if suppressed {
					want = 0
				}
				if evaluations != 1 || len(r.named("tool_done")) != want || len(r.named("overflow_done")) != 0 || lastResponse(t, r)["tool_call_count"] != 2 {
					t.Fatal(evaluations, r.names(), lastResponse(t, r))
				}
				if !suppressed && (r.props(t, "tool_done")["kind"] != "list" || r.props(t, "tool_done")["result_size_bytes"] != 10) {
					t.Fatal(r.props(t, "tool_done"))
				}
			})
		}
	}
}

func TestRound3PausedDeliveryIgnoresTextAndFirstToken(t *testing.T) {
	r := setupAI(t, WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) { return ClassifyResult{}, nil }}))
	client := &pauseReentrantClient{recorder: r}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	client.reenter = func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "late text"})
		i.firstTokenAt(time.Now())
		i.keepMessages([]any{"late messages"})
	}
	i.Pause(PauseOptions{})
	i.Resume()
	if i.answer != "" || i.messages != nil || i.firstToken >= 0 {
		t.Fatal("paused delivery retained text/timing", i.answer, i.messages, i.firstToken)
	}
	i.End(EndOptions{})
}

func TestRound3CreditsPauseKeepsOutputText(t *testing.T) {
	var i *Interaction
	setupAI(t, WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) { return ClassifyResult{}, nil }}), WithCredits(func(CreditCall) (*float64, error) { i.Pause(PauseOptions{}); return Ptr(1.0), nil }))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "answer"})
	if i.answer != "answer" || i.Paused() {
		t.Fatal("credits callback lost answer", i.answer)
	}
	i.End(EndOptions{})
}

func TestRound3EmptyToolsStayPaused(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.Pause(PauseOptions{})
	i.RecordToolCalls()
	if !i.Paused() {
		t.Fatal("empty tools resumed")
	}
	i.End(EndOptions{})
}

func TestRound3StaleTTLSelection(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	open.mu.Lock()
	i.opened = time.Now().Add(-time.Hour)
	open.lastSweep = time.Time{}
	open.mu.Unlock()
	selected := expiredInteractions(time.Minute)
	if len(selected) != 1 {
		t.Fatal(selected)
	}
	i.Pause(PauseOptions{})
	i.Resume()
	selected[0].End(EndOptions{Status: StatusIncomplete, endReason: endReasonTTL})
	if i.Ended() {
		t.Fatal("stale TTL selection ended resumed run")
	}
	i.End(EndOptions{})
}

func TestRound3SnapshotRejectsInvalidNewFields(t *testing.T) {
	for _, field := range []string{"report_count", "successful_call_sequence", "pending_compactions"} {
		for _, value := range []any{nil, -1, 1.5, "1", true, int64(9007199254740992)} {
			setupAI(t)
			raw := map[string]any{"interaction_id": "old", "conversation_id": "c", "trigger": "user", "pause_count": 1, "total_paused_ms": 0, "paused_at": 1, field: value}
			var s PausedInteraction
			if json.Unmarshal([]byte(mustJSON(raw)), &s) != nil {
				continue
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &s, UserID: "u"})
			if i.ID() == "old" {
				t.Fatalf("accepted %s %v", field, value)
			}
			i.End(EndOptions{})
		}
	}
	for _, record := range []map[string]any{
		{"overflow": "true"}, {"rule_outcome": true},
		{"rule_outcome": map[string]any{"suppressed": "false", "properties": map[string]any{}}},
		{"rule_outcome": map[string]any{"suppressed": false, "properties": []any{}}},
		{"rule_outcome": map[string]any{"suppressed": false, "properties": map[string]any{}, "result_size_bytes": -1}},
		{"rule_outcome": map[string]any{"suppressed": false, "properties": map[string]any{}, "result_size_bytes": 1.5}},
	} {
		setupAI(t)
		s := PausedInteraction{InteractionID: "old", ConversationID: "c", Trigger: TriggerUser, PauseCount: 1, PendingTools: []map[string]any{record}}
		_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &s, UserID: "u"})
		if i.ID() == "old" {
			t.Fatal("accepted invalid tool", record)
		}
		i.End(EndOptions{})
	}
}

func TestRound3LegacyPendingSnapshot(t *testing.T) {
	evaluated := 0
	r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"tool": {Event: "tool_done", When: func(map[string]any, any) bool { evaluated++; return true }}}}))
	s := PausedInteraction{InteractionID: "legacy", ConversationID: "c", Trigger: TriggerUser, UserID: "u", PauseCount: 1, CompactionCount: 1,
		PendingCalls: []map[string]any{{"provider": "openai", "model": "m", "call_count": 3, "last": true, "last_ok": true}},
		PendingTools: []map[string]any{{"tool_name": "tool", "call_count": 1}}}
	_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
	if i.ID() != "legacy" || i.callCount != 3 {
		t.Fatal("legacy restore changed")
	}
	i.End(EndOptions{})
	if evaluated != 1 || len(r.named("tool_done")) != 1 || lastResponse(t, r)["compaction_count"] != 1 {
		t.Fatal(r.names(), lastResponse(t, r))
	}
}

func TestRound3DuplicateKeepsOutputText(t *testing.T) {
	setupAI(t, WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) { return ClassifyResult{}, nil }}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "same"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "same", OutputText: "answer"})
	if i.answer != "answer" {
		t.Fatal("duplicate enrichment lost output text")
	}
	i.End(EndOptions{})
}
