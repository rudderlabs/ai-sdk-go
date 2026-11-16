package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func taggedRuleCall(id, parent string, usage bool, response, answer string) LLMCall {
	start := time.Now().Add(-time.Second)
	return LLMCall{Integration: "tracing", Provider: "openai", Model: "m", ResponseID: response, InputTokens: 7, OutputText: answer,
		StartedAt: start, EndedAt: start.Add(time.Millisecond),
		spanSource: &hook.SpanSource{TraceID: "trace", SpanID: id, ParentSpanID: parent, ResponseID: response, ResponseModel: "m", Usage: usage,
			Start: float64(start.UnixMilli()), End: float64(start.Add(time.Millisecond).UnixMilli()), Ancestors: []string{}}}
}

func TestSpanRulesEndDrainsToolHooks(t *testing.T) {
	var i *Interaction
	var hooks []string
	r := setupAI(t, WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
		"a": {Event: "Tool A", Properties: func(map[string]any, any) map[string]any {
			hooks = append(hooks, "a")
			i.RecordToolCalls(ToolCall{ID: "b", Name: "b"})
			return nil
		}},
		"b": {Event: "Tool B", Properties: func(map[string]any, any) map[string]any {
			hooks = append(hooks, "b")
			i.RecordCall(taggedRuleCall("child", "parent", true, "child-response", ""))
			return nil
		}},
	}}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.RecordCall(taggedRuleCall("parent", "", true, "", ""))
	i.RecordToolCalls(ToolCall{ID: "a", Name: "a"})
	i.End(EndOptions{})
	if !slices.Equal(hooks, []string{"a", "b"}) {
		t.Fatalf("tool hooks = %v, want [a b]", hooks)
	}
	if len(r.named("Tool A")) != 1 || len(r.named("Tool B")) != 1 || lastResponse(t, r)["llm_call_count"] != 1 {
		t.Fatal(r.names(), lastResponse(t, r))
	}
	calls := r.named("ai_llm_response_received")
	if len(calls) != 1 || calls[0].Properties["response_id"] != "child-response" || calls[0].Properties["call_index"] != 1 {
		t.Fatal(calls)
	}
}

func TestSpanRulesEndFloorsPrepWindowToMilliseconds(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	start := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	i.eventClock = func() time.Time { return start.Add(10300 * time.Microsecond) }
	prep := taggedRuleCall("prep", "", false, "", "")
	prep.StartedAt, prep.EndedAt = start, start.Add(time.Millisecond)
	prep.InputTokens = 0
	prep.spanSource.Start, prep.spanSource.End = float64(start.UnixMilli()), float64(start.UnixMilli()+1)
	prep.spanSource.LLM = true
	usage := taggedRuleCall("usage", "", true, "usage-response", "")
	usage.StartedAt, usage.EndedAt = start.Add(10100*time.Microsecond), start.Add(10200*time.Microsecond)
	usage.spanSource.Start, usage.spanSource.End = float64(start.UnixMilli())+10.1, float64(start.UnixMilli())+10.2
	i.RecordCall(prep)
	i.RecordCall(usage)
	i.End(EndOptions{})
	if lastResponse(t, r)["llm_call_count"] != 2 {
		t.Fatal(lastResponse(t, r))
	}
	calls := r.named("ai_llm_response_received")
	if len(calls) != 2 || calls[0].Properties["call_index"] != 1 || calls[1].Properties["call_index"] != 2 {
		t.Fatal(calls)
	}
}

func TestSpanRulesPendingEvidenceAcrossPause(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			client := &pauseReentrantClient{recorder: r}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Analytics: client})
			client.reenter = func() {
				i.recordSpanAncestry("trace", "bridge", "parent")
				i.RecordCall(taggedRuleCall("parent", "", false, "", ""))
				i.RecordCall(taggedRuleCall("child", "bridge", true, "child-response", ""))
			}
			snapshot, ok := i.Pause(PauseOptions{})
			if !ok || len(snapshot.PendingCalls) != 2 {
				t.Fatal(snapshot, ok)
			}
			source := restoredSpanSource(snapshot.PendingCalls[1]["span_source"])
			if source == nil || !slices.Contains(source.Ancestors, "parent") {
				t.Fatal(snapshot.PendingCalls)
			}
			if restore {
				b, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var decoded PausedInteraction
				if err := json.Unmarshal(b, &decoded); err != nil {
					t.Fatal(err)
				}
				_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &decoded})
			}
			i.Resume()
			i.End(EndOptions{})
			answers := r.named("ai_response_received")
			if len(answers) != 2 || answers[0].Properties["llm_call_count"] != 0 || answers[1].Properties["llm_call_count"] != 1 {
				t.Fatal(answers)
			}
			calls := r.named("ai_llm_response_received")
			if len(calls) != 1 || calls[0].Properties["call_index"] != 1 || calls[0].Properties["response_id"] != "child-response" {
				t.Fatal(calls)
			}
			for _, event := range calls {
				if _, exists := event.Properties["span_source"]; exists {
					t.Fatal(event)
				}
			}
		})
	}
}

func TestSpanRulesPublishedParentStaysCounted(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	parent := taggedRuleCall("parent", "", true, "parent-response", "")
	i.RecordCall(parent)
	wrapper := parent
	wrapper.Integration, wrapper.spanSource = "openai", nil
	i.RecordCall(wrapper)
	if len(r.named("ai_llm_response_received")) != 1 {
		t.Fatal("matched parent event not emitted")
	}
	i.RecordCall(taggedRuleCall("child", "parent", true, "child-response", ""))
	i.End(EndOptions{})
	if len(r.named("ai_llm_response_received")) != 2 || lastResponse(t, r)["llm_call_count"] != 2 {
		t.Fatal(r.names(), lastResponse(t, r))
	}
}

func TestSpanRulesWrapperTwinStaysCounted(t *testing.T) {
	for _, emitCalls := range []bool{false, true} {
		for _, spanFirst := range []bool{false, true} {
			for _, prepTwin := range []bool{false, true} {
				t.Run(fmt.Sprintf("emitCalls=%t/spanFirst=%t/prepTwin=%t", emitCalls, spanFirst, prepTwin), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(emitCalls))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
					start := time.Now().Add(-time.Second)
					spanCall := func(id, parent string, from, to int, tokens int, response string) LLMCall {
						call := taggedRuleCall(id, parent, tokens > 0, response, "")
						call.StartedAt, call.EndedAt = start.Add(time.Duration(from)*time.Millisecond), start.Add(time.Duration(to)*time.Millisecond)
						call.InputTokens = tokens
						call.spanSource.Start, call.spanSource.End = float64(call.StartedAt.UnixMilli()), float64(call.EndedAt.UnixMilli())
						call.spanSource.LLM = true
						return call
					}
					// Block call events until End, including the authoritative wrapper event.
					i.RecordCall(spanCall("pending-prep", "", 0, 50, 0, ""))
					twin := spanCall("twin", "", 100, 900, 7, "wrapper-response")
					child := spanCall("child", "twin", 200, 800, 1, "child-response")
					wrapper := twin
					wrapper.Integration, wrapper.spanSource, wrapper.InputTokens = "openai", nil, 100
					if prepTwin {
						twin.InputTokens, twin.ResponseID = 0, ""
						twin.spanSource.Usage, twin.spanSource.ResponseID = false, ""
						child.spanSource.ParentSpanID = ""
					}
					if spanFirst {
						i.RecordCall(twin)
						i.RecordCall(wrapper)
					} else {
						i.RecordCall(wrapper)
						i.RecordCall(twin)
					}
					i.RecordCall(child)
					if len(i.calls) != 3 || len(r.named("ai_llm_response_received")) != 0 {
						t.Fatal("expected three pending records and no published call events")
					}
					i.End(EndOptions{})
					answer := lastResponse(t, r)
					if answer["llm_call_count"] != 2 || answer["total_input_tokens"] != 101 || answer["total_tokens"] != 101 {
						t.Fatal(answer)
					}
					calls := r.named("ai_llm_response_received")
					if !emitCalls {
						if len(calls) != 0 {
							t.Fatal(calls)
						}
						return
					}
					if len(calls) != 2 || calls[0].Properties["response_id"] != "wrapper-response" || calls[0].Properties["input_tokens"] != 100 || calls[0].Properties["call_index"] != 1 || calls[1].Properties["call_index"] != 2 {
						t.Fatal(calls)
					}
				})
			}
		}
	}
}

func TestSpanRulesSkippedParentAnswerAndIndices(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.RecordCall(taggedRuleCall("child", "parent", true, "child-response", "child answer"))
	i.RecordCall(taggedRuleCall("parent", "", true, "", "redundant parent answer"))
	i.End(EndOptions{})
	if lastResponse(t, r)["response_text"] != "child answer" || lastResponse(t, r)["llm_call_count"] != 1 {
		t.Fatal(lastResponse(t, r))
	}
	calls := r.named("ai_llm_response_received")
	if len(calls) != 1 || calls[0].Properties["call_index"] != 1 || calls[0].Properties["output_text"] != "child answer" {
		t.Fatal(calls)
	}
}

func TestSpanRulesPauseFiltersOnceAndClearsAncestry(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.RecordCall(taggedRuleCall("parent", "", false, "", ""))
	i.RecordCall(taggedRuleCall("child", "parent", true, "child-response", ""))
	if _, ok := i.Pause(PauseOptions{}); !ok {
		t.Fatal("pause failed")
	}
	if len(i.calls) != 0 || len(i.spanParents) != 0 || lastResponse(t, r)["llm_call_count"] != 1 {
		t.Fatal(lastResponse(t, r))
	}
	i.Resume()
	i.End(EndOptions{})
	if lastResponse(t, r)["llm_call_count"] != 0 || len(r.named("ai_llm_response_received")) != 1 {
		t.Fatal(r.names(), lastResponse(t, r))
	}
}

func TestSpanRulesInvalidSnapshotEvidenceCountsConservatively(t *testing.T) {
	good := spanSourceSnapshot(taggedRuleCall("span", "", true, "response", "").spanSource)
	for _, field := range []string{"spanId", "start", "usage", "ancestors"} {
		t.Run(field, func(t *testing.T) {
			b, _ := json.Marshal(good)
			var broken map[string]any
			_ = json.Unmarshal(b, &broken)
			broken[field] = nil
			if restoredSpanSource(broken) != nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	if restoredSpanSource(good) == nil || restoredSpanSource(nil) != nil {
		t.Fatal("valid or legacy evidence mishandled")
	}
}
