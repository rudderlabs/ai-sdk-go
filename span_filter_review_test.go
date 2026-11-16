package rudderstackai

import (
	"context"
	"testing"
	"time"
)

func TestSpanRulesPreserveNewerAnswer(t *testing.T) {
	for _, source := range []string{"folded span", "folded wrapper", "keep answer", "other wrapper"} {
		t.Run(source, func(t *testing.T) {
			limit := 10
			if source == "folded span" || source == "folded wrapper" {
				limit = 2
			}
			answers := make(chan string, 2)
			r := setupAI(t, WithCaptureContent(true), WithLimits(Limits{MaxCallsPerInteraction: limit}),
				WithClassifier(&ClassifierOptions{Classify: func(_ context.Context, turn ClassifyTurn) (ClassifyResult, error) {
					answers <- turn.Answer
					return ClassifyResult{}, nil
				}}), WithEvaluator(func(_ context.Context, turn EvaluationTurn) ([]Score, error) {
					answers <- turn.Answer
					return nil, nil
				}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
			i.RecordCall(taggedRuleCall("child", "parent", true, "child-response", "child answer"))
			i.RecordCall(taggedRuleCall("parent", "", true, "", "parent answer"))
			if source == "keep answer" {
				i.mu.Lock()
				i.keepAnswerLocked("new answer", false)
				i.mu.Unlock()
			} else {
				call := taggedRuleCall("new", "", false, "", "new answer")
				call.StartedAt, call.EndedAt = time.Now(), time.Now()
				if source != "folded span" {
					call.Integration, call.spanSource = "openai", nil
				}
				i.RecordCall(call)
			}
			i.End(EndOptions{})
			for range 2 {
				select {
				case got := <-answers:
					if got != "new answer" {
						t.Errorf("answer = %q, want new answer", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("answer callback did not run")
				}
			}
			want := "child answer"
			if source == "folded span" {
				want = "new answer"
			}
			if got := lastResponse(t, r)["response_text"]; got != want {
				t.Errorf("response = %v, want %s", got, want)
			}
		})
	}
}

func TestSpanRulesFilterAfterCaptureCallbacks(t *testing.T) {
	for _, ending := range []bool{false, true} {
		for _, source := range []string{"redactor", "tool callback", "second redaction"} {
			t.Run(map[bool]string{false: "pause", true: "end"}[ending]+"/"+source, func(t *testing.T) {
				var i *Interaction
				capturing, added := false, false
				add := func() {
					if added {
						return
					}
					added = true
					if ending {
						i.End(EndOptions{})
						if _, ok := i.Pause(PauseOptions{}); ok {
							t.Fatal("capture callback exposed an unfinished pause")
						}
					}
					call := taggedRuleCall("child", "parent", true, "child-response", "child answer")
					i.RecordCall(call)
				}
				r := setupAI(t, WithCaptureContent(true), WithLLMCallEvents(true), WithRedactHook(func(text string) (string, error) {
					if capturing {
						if source == "redactor" && text == "parent answer" {
							add()
						}
						if source == "tool callback" && text == "parent answer" && !added {
							i.RecordToolCalls(ToolCall{ID: "lookup", Name: "lookup"})
						}
						if source == "second redaction" && text == "parent answer" {
							add()
						}
						if source == "second redaction" && text == "child answer" {
							prep := taggedRuleCall("late-prep", "", false, "", "")
							prep.InputTokens = 0
							prep.spanSource.LLM = true
							i.RecordCall(prep)
						}
					}
					return "masked " + text, nil
				}), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
					"lookup": {Event: "lookup_done", Properties: func(map[string]any, any) map[string]any { add(); return nil }},
				}}))
				_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
				i.RecordCall(taggedRuleCall("parent", "", true, "", "parent answer"))
				capturing = true
				if ending {
					i.End(EndOptions{})
				} else {
					i.Pause(PauseOptions{})
				}
				response := lastResponse(t, r)
				if response["llm_call_count"] != 1 || response["total_input_tokens"] != 7 || response["response_text"] != "masked child answer" {
					t.Fatal(response)
				}
				calls := r.named("ai_llm_response_received")
				if len(calls) != 1 || calls[0].Properties["call_index"] != 1 {
					t.Fatal(calls)
				}
				if !ending {
					i.End(EndOptions{})
				}
			})
		}
	}
}
