package rudderstackai

import (
	"context"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func startForAdapter(startedAt time.Time) *Interaction {
	_, i := hook.StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true}, "claude-managed-agents", nil, startedAt)
	return i.(*Interaction)
}

func TestAdapterStartAndPrimaryModel(t *testing.T) {
	r := setupAI(t)
	i := startForAdapter(time.Now().Add(-2 * time.Second))
	i.RecordCall(LLMCall{Provider: "anthropic", Model: "claude-haiku-5-5", InputTokens: 1})
	i.RecordCall(LLMCall{Provider: "anthropic", AgentName: "outcome_grader", InputTokens: 1})
	hook.PrimaryModel(i, "claude-haiku-5-5", "anthropic")
	hook.KeepAnswer(i, "answer")
	hook.Adopt(i, "other", nil) // the first tag wins
	i.End(EndOptions{})
	p := r.response(t)
	if p["primary_model"] != "claude-haiku-5-5" || p["latency_ms"].(int64) < 1900 {
		t.Fatal(p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if integrationOf(m) != "claude-managed-agents" {
			t.Fatalf("integration %q", integrationOf(m))
		}
	}
}

func TestAdapterPrimaryModelYieldsToALaterCall(t *testing.T) {
	r := setupAI(t)
	i := startForAdapter(time.Time{})
	hook.PrimaryModel(i, "claude-haiku-5-5", "anthropic")
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-5", InputTokens: 1})
	i.End(EndOptions{})
	if p := r.response(t); p["primary_model"] != "gpt-5" {
		t.Fatal(p)
	}
}

func TestAdapterPausedHintCarriesItsReason(t *testing.T) {
	r := setupAI(t)
	i := startForAdapter(time.Time{})
	hook.HintOutcome(i, string(StatusPaused), "", string(PauseApproval))
	i.End(EndOptions{})
	if p := r.response(t); p["response_status"] != "paused" || p["pause_reason"] != "approval" {
		t.Fatal(p)
	}
	if hook.ContextConversation(WithValues(context.Background(), ContextValues{ConversationID: "c1"})) != "c1" ||
		hook.ContextConversation(context.Background()) != "" {
		t.Fatal("ContextConversation")
	}
	hook.WarnOnce("adapter-hooks-test", "warned once")
	if hook.MaxCallsPerInteraction() <= 0 {
		t.Fatal("MaxCallsPerInteraction")
	}
}
