package rudderstackai

import (
	"context"
	"testing"
	"time"
)

func TestSpanRulesNestedResponseIDs(t *testing.T) {
	for _, tc := range []struct {
		name, parentResponseID, childResponseID string
		count                                   int
	}{
		{"distinct", "parent-response", "child-response", 2},
		{"shared", "shared-response", "shared-response", 1},
		{"parent absent", "", "child-response", 1},
		{"child absent", "parent-response", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
			i.recordSpanAncestry("trace", "parent", "")
			i.recordSpanAncestry("trace", "child", "parent")
			start := time.Now().Add(-time.Second)
			for _, call := range []LLMCall{
				taggedRuleCall("child", "parent", true, tc.childResponseID, ""),
				taggedRuleCall("parent", "", true, tc.parentResponseID, ""),
			} {
				call.InputTokens, call.OutputTokens = 10, 2
				call.StartedAt, call.EndedAt = start, start.Add(time.Millisecond)
				call.spanSource.Start, call.spanSource.End = float64(start.UnixMilli()), float64(call.EndedAt.UnixMilli())
				i.RecordCall(call)
			}
			i.End(EndOptions{})
			response := lastResponse(t, r)
			if response["llm_call_count"] != tc.count || response["total_input_tokens"] != tc.count*10 || response["total_output_tokens"] != tc.count*2 {
				t.Fatal(response)
			}
			if calls := r.named("ai_llm_response_received"); len(calls) != tc.count {
				t.Fatal(calls)
			}
		})
	}
}

func TestSpanRulesMissingBridgeAncestryKeepsCalls(t *testing.T) {
	for _, missing := range []string{"evicted", "unknown"} {
		for _, rule := range []string{"prep", "nested"} {
			t.Run(missing+"/"+rule, func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxMapEntries: 1}))
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
				i.recordSpanAncestry("trace", "A", "")
				parent := taggedRuleCall("A", "", false, "", "")
				parent.InputTokens, parent.spanSource.LLM = 0, true
				if rule == "nested" {
					parent.OutputText, parent.spanSource.Output = "A answer", true
				}
				i.RecordCall(parent)
				if missing == "evicted" {
					i.recordSpanAncestry("trace", "bridge", "A")
				}
				i.recordSpanAncestry("trace", "child", "bridge")
				if _, present := i.spanParents["trace:bridge"]; present {
					t.Fatal("bridge edge was not evicted")
				}
				child := taggedRuleCall("child", "bridge", true, "", "")
				if rule == "prep" {
					child.Model, child.spanSource.ResponseModel = "other-model", "other-model"
				}
				i.RecordCall(child)
				i.End(EndOptions{})
				if response := lastResponse(t, r); response["llm_call_count"] != 2 || response["total_input_tokens"] != 7 {
					t.Fatal(response)
				}
				if calls := r.named("ai_llm_response_received"); len(calls) != 2 {
					t.Fatal(calls)
				}
			})
		}
	}
}

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
				// Recording order must not shift the observed span windows: prep
				// suppression allows usage to start at most 1 ms before the prep span.
				start := time.Unix(1_700_000_000, 0)
				callAt := func(id, parent string, usage bool, response, answer string) LLMCall {
					call := taggedRuleCall(id, parent, usage, response, answer)
					call.StartedAt, call.EndedAt = start, start.Add(time.Millisecond)
					call.spanSource.Start, call.spanSource.End = float64(call.StartedAt.UnixMilli()), float64(call.EndedAt.UnixMilli())
					return call
				}
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
					call := callAt("child", "parent", true, "child-response", "child answer")
					i.RecordCall(call)
				}
				r := setupAI(t, WithCaptureContent(true), WithLLMCallEvents(true), WithRedactHook(func(text string) (string, error) {
					if capturing {
						if source == "redactor" && text == "parent answer" {
							add()
						}
						// No ID: the pause does not wait on it (SDK-6017), so its rules run at pause.
						if source == "tool callback" && text == "parent answer" && !added {
							i.RecordToolCalls(ToolCall{Name: "lookup"})
						}
						if source == "second redaction" && text == "parent answer" {
							add()
						}
						if source == "second redaction" && text == "child answer" {
							prep := callAt("late-prep", "", false, "", "")
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
				i.eventClock = func() time.Time { return start.Add(time.Second) }
				i.RecordCall(callAt("parent", "", true, "", "parent answer"))
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
