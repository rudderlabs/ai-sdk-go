package rudderstackai

// SDK-6017 (D2): a tool call the pause waits on is counted once, in the segment that runs it.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func sdk6017Call(agentName string, inputTokens int) LLMCall {
	return LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: inputTokens, AgentName: agentName}
}

func sdk6017Start() *Interaction {
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	return i
}

// sdk6017Continue pauses, then continues in process or in an interaction restored from the JSON snapshot.
func sdk6017Continue(t *testing.T, i *Interaction, restore bool) *Interaction {
	t.Helper()
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause")
	}
	return round3Resume(t, i, snapshot, restore)
}

func sdk6017ToolCounts(r *recorder) []int {
	out := []int{}
	for _, e := range r.named("ai_response_received") {
		out = append(out, int(sdk6111Num(e.Properties["tool_call_count"])))
	}
	return out
}

func sdk6017ToolEvents(r *recorder, event string) [][2]any {
	out := [][2]any{}
	for _, e := range r.named(event) {
		out = append(out, [2]any{e.Properties["tool_call_id"], e.Properties["tool_status"]})
	}
	return out
}

func sdk6017Expect(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func sdk6017Rules(tools map[string]string) ConfigOption {
	rules := map[string]ToolEventRule{}
	for name, event := range tools {
		rules[name] = ToolEventRule{Event: event}
	}
	return WithEventRules(EventRules{Tools: rules})
}

func TestSDK6017AwaitedTool(t *testing.T) {
	for _, restore := range []bool{false, true} {
		mode := map[bool]string{false: "in-process", true: "json"}[restore]
		t.Run(mode+"/is counted once, after resume, with its declared event fired once", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}), WithToolBreakdown(true))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			i := sdk6017Continue(t, first, restore)
			i.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me", Result: "ok", Error: Ptr(false)})
			i.RecordCall(sdk6017Call("", 20))
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			segments := r.named("ai_response_received")
			if _, ok := segments[0].Properties["tool_breakdown"]; ok {
				t.Fatalf("paused tool_breakdown = %v, want absent", segments[0].Properties["tool_breakdown"])
			}
			sdk6017Expect(t, "tool_breakdown", mustJSON(segments[1].Properties["tool_breakdown"]), `[{"call_count":1,"error_count":0,"tool_name":"approve_me"}]`)
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "success"}})
		})

		t.Run(mode+"/counts a denied execution once, with its error, after resume", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}), WithToolBreakdown(true))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			i := sdk6017Continue(t, first, restore)
			i.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me", Error: Ptr(true), ErrorType: "denied"})
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "tool_breakdown", mustJSON(lastResponse(t, r)["tool_breakdown"]), `[{"call_count":1,"error_count":1,"error_types":{"denied":1},"tool_name":"approve_me"}]`)
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "denied"}})
		})

		t.Run(mode+"/merges a RecordToolResult after resume into the carried tool", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			i := sdk6017Continue(t, first, restore)
			i.RecordToolResult("call_1", ToolResult{Result: "ok", Error: Ptr(false), Duration: Ptr(5 * time.Millisecond)})
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "success"}})
		})

		t.Run(mode+"/counts a tool still unresolved at the resumed End there, as unknown", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			i := sdk6017Continue(t, first, restore)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "unknown"}})
		})

		t.Run(mode+"/counts a tool from an agent's earlier call at pause and carries one from its latest call", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"lookup": "Looked Up"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("researcher", 10))
			first.RecordToolCalls(ToolCall{ID: "early", Name: "lookup"})
			first.RecordCall(sdk6017Call("researcher", 10))
			first.RecordToolCalls(ToolCall{ID: "late", Name: "lookup"})
			i := sdk6017Continue(t, first, restore)
			sdk6017Expect(t, "Looked Up at pause", sdk6017ToolEvents(r, "Looked Up"), [][2]any{{"early", "unknown"}})
			i.RecordToolCalls(ToolCall{ID: "late", Name: "lookup", Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{1, 1})
			sdk6017Expect(t, "Looked Up", sdk6017ToolEvents(r, "Looked Up"), [][2]any{{"early", "unknown"}, {"late", "success"}})
		})

		t.Run(mode+"/inherits the previous call's agent and is re-evaluated at a second pause", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"search": "Searched"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("researcher", 10))
			// No agent of its own: the researcher's, so the planner's later call leaves it pending.
			first.RecordToolCalls(ToolCall{ID: "r1", Name: "search"})
			first.RecordCall(sdk6017Call("planner", 10))
			second := sdk6017Continue(t, first, restore)
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0})
			second.RecordCall(sdk6017Call("planner", 10))
			third := sdk6017Continue(t, second, restore)
			// Still no later researcher call: carried again.
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 0})
			third.RecordCall(sdk6017Call("researcher", 10))
			third.Pause(PauseOptions{})
			// The researcher moved on without running it: counted at this pause, as unknown.
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 0, 1})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"r1", "unknown"}})
		})

		t.Run(mode+"/runs its declared rules when it settles after resume, not at pause", func(t *testing.T) {
			whenCalls, propertiesCalls := 0, 0
			r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": {
				Event: "Searched",
				When:  func(map[string]any, any) bool { whenCalls++; return true },
				Properties: func(args map[string]any, result any) map[string]any {
					propertiesCalls++
					query, ok := args["q"].(string)
					if !ok {
						query = "none"
					}
					return map[string]any{"query": query, "kind": fmt.Sprintf("%T", result)}
				},
			}}}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "search", Args: map[string]any{"q": "raw-query"}})
			snapshot, ok := first.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause")
			}
			if whenCalls != 0 || propertiesCalls != 0 {
				t.Fatalf("rules ran at pause: when %d, properties %d", whenCalls, propertiesCalls)
			}
			if len(snapshot.PendingTools) != 1 {
				t.Fatalf("pending_tools = %v, want 1 record", snapshot.PendingTools)
			}
			if outcome, ok := snapshot.PendingTools[0]["rule_outcome"]; ok {
				t.Fatalf("rule_outcome = %v, want absent", outcome)
			}
			if strings.Contains(mustJSON(snapshot), "raw-query") {
				t.Fatal("snapshot carries the tool arguments")
			}
			i := round3Resume(t, first, snapshot, restore)
			i.RecordToolCalls(ToolCall{ID: "call_1", Name: "search", Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			if whenCalls != 1 {
				t.Fatalf("When ran %d times, want 1", whenCalls)
			}
			// Snapshots carry no arguments: after a JSON restore only the execution record's are seen.
			want := map[bool]string{false: "raw-query", true: "none"}[restore]
			p := r.props(t, "Searched")
			if p["query"] != want || p["kind"] != "string" || p["tool_status"] != "success" {
				t.Fatalf("Searched = %v, want query %q, kind string, tool_status success", p, want)
			}
		})

		t.Run(mode+"/counts tools without an ID or with an outcome at pause, as before", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"lookup": "Looked Up"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{Name: "lookup"}, ToolCall{ID: "done", Name: "lookup", Result: "ok", Error: Ptr(false)})
			i := sdk6017Continue(t, first, restore)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{2, 0})
			sdk6017Expect(t, "Looked Up", sdk6017ToolEvents(r, "Looked Up"), [][2]any{{"done", "success"}, {nil, "unknown"}})
		})
	}
}

func TestSDK6017NeverCountsAPendingToolOfARunThatIsNotResumed(t *testing.T) {
	r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
	i := sdk6017Start()
	i.RecordCall(sdk6017Call("", 10))
	i.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
	snapshot, _ := i.Pause(PauseOptions{})
	sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0})
	if n := len(r.named("Approved")); n != 0 {
		t.Fatalf("Approved = %d, want 0", n)
	}
	if len(snapshot.PendingTools) != 1 {
		t.Fatalf("pending_tools = %v, want 1 record", snapshot.PendingTools)
	}
	p := snapshot.PendingTools[0]
	if p["tool_call_id"] != "call_1" || p["tool_name"] != "approve_me" || p["settled"] != false {
		t.Fatalf("pending tool = %v", p)
	}
}

func TestSDK6017CallRecordedDuringPauseDeliveryEndsTheWait(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), sdk6017Rules(map[string]string{"lookup": "Looked Up"}))
	client := &sdk6111ReenterClient{recorder: r, event: "ai_llm_response_received"}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	// A timed span's call event waits until pause delivers it.
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10, AgentName: "a"}, 100))
	i.RecordToolCalls(ToolCall{ID: "t1", Name: "lookup"})
	// Its agent's next call, recorded during that delivery, ends the wait: the paused segment counts it, as in Node.
	client.reenter = func() {
		i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 20, AgentName: "a"}, 300))
	}
	i.Pause(PauseOptions{})
	i.Resume()
	i.End(EndOptions{})
	sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{1, 0})
	sdk6017Expect(t, "Looked Up", sdk6017ToolEvents(r, "Looked Up"), [][2]any{{"t1", "unknown"}})
}

// sdk6017RestoredWithoutAgentKey restores from a snapshot whose pending tool lacks agent_key, as
// snapshots written before that field do.
func sdk6017RestoredWithoutAgentKey(t *testing.T, agentName string) *Interaction {
	t.Helper()
	first := sdk6017Start()
	first.RecordCall(sdk6017Call("planner", 10))
	first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me", AgentName: agentName})
	snapshot, _ := first.Pause(PauseOptions{})
	var stored PausedInteraction
	if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.PendingTools) != 1 || stored.PendingTools[0]["agent_key"] != "planner" {
		t.Fatalf("pending_tools = %v, want one record with agent_key planner", stored.PendingTools)
	}
	delete(stored.PendingTools[0], "agent_key")
	_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	return i
}

func TestSDK6017SnapshotWrittenBeforeAgentKey(t *testing.T) {
	t.Run("restores its pending tool and counts it once with its outcome", func(t *testing.T) {
		r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
		i := sdk6017RestoredWithoutAgentKey(t, "")
		i.RecordToolResult("call_1", ToolResult{Error: Ptr(false)})
		i.End(EndOptions{})
		sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
		sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "success"}})
	})

	t.Run("keys a pending tool by its own agent_name", func(t *testing.T) {
		r := setupAI(t)
		i := sdk6017RestoredWithoutAgentKey(t, "planner")
		// The planner's next call settles its wait, so a later pause counts the tool.
		i.RecordCall(sdk6017Call("planner", 10))
		i.Pause(PauseOptions{})
		sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
	})
}

// As in Node, a tool settled while the run is paused sends its event only once the run goes on.
func TestSDK6017HoldsTheEventOfAToolSettledWhilePaused(t *testing.T) {
	r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
	i := sdk6017Start()
	i.RecordCall(sdk6017Call("", 10))
	i.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
	if _, ok := i.Pause(PauseOptions{}); !ok {
		t.Fatal("pause")
	}
	i.RecordToolResult("call_1", ToolResult{Error: Ptr(false)})
	sdk6017Expect(t, "Approved while paused", len(r.named("Approved")), 0)
	i.End(EndOptions{})
	sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "success"}})
	sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
}

func TestSDK6017WritesAnEmptyAgentKeyForAnAgentlessToolGivenAnAgentLater(t *testing.T) {
	setupAI(t)
	i := sdk6017Start()
	i.RecordCall(sdk6017Call("", 10))
	i.RecordToolCalls(ToolCall{ID: "keyless", Name: "approve_me"})
	i.RecordToolCalls(ToolCall{ID: "keyless", Name: "approve_me", AgentName: "researcher"})
	i.RecordToolCalls(ToolCall{ID: "plain", Name: "approve_me"})
	i.RecordToolCalls(ToolCall{ID: "own", Name: "approve_me", AgentName: "planner"})
	snapshot, _ := i.Pause(PauseOptions{})
	var stored PausedInteraction
	if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
		t.Fatal(err)
	}
	got := [][3]any{}
	for _, p := range stored.PendingTools {
		got = append(got, [3]any{p["tool_call_id"], p["agent_name"], p["agent_key"]})
	}
	// "plain": the key equals what restore falls back to, so it is left out.
	sdk6017Expect(t, "pending_tools", got, [][3]any{{"keyless", "researcher", ""}, {"plain", nil, nil}, {"own", "planner", "planner"}})
}

func TestSDK6017DurationOnlyResultEndsTheWait(t *testing.T) {
	for _, restore := range []bool{false, true} {
		mode := map[bool]string{false: "in-process", true: "json"}[restore]
		t.Run(mode+"/recorded while paused", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			first.Pause(PauseOptions{})
			first.RecordToolResult("call_1", ToolResult{Duration: Ptr(5 * time.Millisecond)})
			// A fresh snapshot: the one taken at the pause predates the result.
			i := sdk6017Continue(t, first, restore)
			i.Pause(PauseOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "unknown"}})
		})

		t.Run(mode+"/recorded during paused delivery", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
			client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
			_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			client.reenter = func() { first.RecordToolResult("call_1", ToolResult{Duration: Ptr(5 * time.Millisecond)}) }
			i := sdk6017Continue(t, first, restore)
			i.Pause(PauseOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"call_1", "unknown"}})
		})
	}
}

// A final end while paused is not a resume: a tool the pause still waits on never ran.
func TestSDK6017NeverCountsAnAwaitedToolWhenTheRunEndsWhilePaused(t *testing.T) {
	for _, status := range []ResponseStatus{"", StatusCancelled, StatusError} {
		t.Run("status "+string(status), func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}), WithToolBreakdown(true))
			i := sdk6017Start()
			i.RecordCall(sdk6017Call("", 10))
			i.RecordToolCalls(ToolCall{ID: "call_1", Name: "approve_me"})
			i.Pause(PauseOptions{})
			i.End(EndOptions{Status: status})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 0})
			if breakdown, ok := r.named("ai_response_received")[1].Properties["tool_breakdown"]; ok {
				t.Fatalf("final tool_breakdown = %v, want absent", breakdown)
			}
			sdk6017Expect(t, "Approved", len(r.named("Approved")), 0)
		})
	}

	t.Run("counts a tool settled while paused, and not one still waited on", func(t *testing.T) {
		r := setupAI(t, sdk6017Rules(map[string]string{"approve_me": "Approved"}))
		i := sdk6017Start()
		i.RecordCall(sdk6017Call("", 10))
		i.RecordToolCalls(ToolCall{ID: "denied", Name: "approve_me"}, ToolCall{ID: "waiting", Name: "approve_me"})
		i.Pause(PauseOptions{})
		i.RecordToolResult("denied", ToolResult{Error: Ptr(true), ErrorType: "denied"})
		i.End(EndOptions{Status: StatusCancelled})
		sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
		sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"denied", "denied"}})
	})
}

// sdk6017ResultRule passes only the real result, so an outcome frozen without it suppresses the event.
func sdk6017ResultRule(calls *[]any) ConfigOption {
	return WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": {Event: "Searched", When: func(_ map[string]any, result any) bool {
		*calls = append(*calls, result)
		return result == "raw-result"
	}}}})
}

func TestSDK6017DeclaredRulesAroundAPause(t *testing.T) {
	for _, restore := range []bool{false, true} {
		mode := map[bool]string{false: "in-process", true: "json"}[restore]
		// Continues from a fresh snapshot: the one taken at the pause predates what was recorded since.
		continued := func(t *testing.T, i *Interaction) *Interaction {
			t.Helper()
			snapshot, ok := i.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause")
			}
			if strings.Contains(mustJSON(snapshot), "raw-result") {
				t.Fatalf("snapshot carries the result: %s", mustJSON(snapshot))
			}
			return round3Resume(t, i, snapshot, restore)
		}

		t.Run(mode+"/run when a tool recorded unresolved during paused delivery settles after resume", func(t *testing.T) {
			var when []any
			r := setupAI(t, sdk6017ResultRule(&when))
			client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
			_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			first.RecordCall(sdk6017Call("", 10))
			client.reenter = func() { first.RecordToolCalls(ToolCall{ID: "late", Name: "search"}) }
			i := continued(t, first)
			sdk6017Expect(t, "when calls at pause", len(when), 0)
			i.RecordToolResult("late", ToolResult{Result: "raw-result", Error: Ptr(false)})
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"late", "success"}})
			sdk6017Expect(t, "when calls", when, []any{"raw-result"})
		})

		t.Run(mode+"/are frozen with the result when an awaited tool settles during paused delivery", func(t *testing.T) {
			var when []any
			r := setupAI(t, sdk6017ResultRule(&when))
			client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
			_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "search"})
			client.reenter = func() {
				first.RecordToolResult("call_1", ToolResult{Result: "raw-result", Error: Ptr(false)})
			}
			i := continued(t, first)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"call_1", "success"}})
			sdk6017Expect(t, "when calls", when, []any{"raw-result"})
		})

		t.Run(mode+"/are frozen with the result of a tool settled while paused, for a later snapshot", func(t *testing.T) {
			var when []any
			r := setupAI(t, sdk6017ResultRule(&when))
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "call_1", Name: "search"})
			first.Pause(PauseOptions{})
			// Held: the run is still paused.
			first.RecordToolResult("call_1", ToolResult{Result: "raw-result", Error: Ptr(false)})
			sdk6017Expect(t, "when calls while paused", len(when), 0)
			i := continued(t, first)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"call_1", "success"}})
			sdk6017Expect(t, "when calls", when, []any{"raw-result"})
		})
	}
}

// A call recorded by another goroutine after the pause's last tool-event pass, and before it
// captures the totals, ends a wait: the tool is counted, so its declared event must be sent too.
func TestSDK6017CallAfterTheLastToolPassStillFiresTheToolItCounts(t *testing.T) {
	for _, restore := range []bool{false, true} {
		mode := map[bool]string{false: "in-process", true: "json"}[restore]
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			// concurrently records agent's next call from another goroutine, once, and waits for it.
			raced := map[string]bool{}
			concurrently := func(agent string) bool {
				if !raced[agent] {
					raced[agent] = true
					done := make(chan struct{})
					go func() {
						defer close(done)
						i.RecordCall(sdk6017Call(agent, 20))
					}()
					<-done
				}
				return true
			}
			r := setupAI(t, WithEventRules(EventRules{Tools: map[string]ToolEventRule{
				// Fired while the pause captures its answer: ends the wait on the researcher's tool.
				"lookup": {Event: "Looked Up", When: func(map[string]any, any) bool { return concurrently("researcher") }},
				// Fired by the pause's next pass, which already skipped the planner's tool: ends that wait.
				"search": {Event: "Searched", When: func(map[string]any, any) bool { return concurrently("planner") }},
				"plan":   {Event: "Planned"},
			}}))
			i = sdk6017Start()
			i.RecordCall(sdk6017Call("researcher", 10))
			i.RecordToolCalls(
				ToolCall{ID: "s1", Name: "search", AgentName: "researcher"},
				ToolCall{ID: "p1", Name: "plan", AgentName: "planner"},
				ToolCall{Name: "lookup"},
			)
			i = sdk6017Continue(t, i, restore)
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{3})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"s1", "unknown"}})
			sdk6017Expect(t, "Planned", sdk6017ToolEvents(r, "Planned"), [][2]any{{"p1", "unknown"}})
			// Counted and sent at the pause: nothing is left for the next segment.
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{3, 0})
			sdk6017Expect(t, "Planned", len(r.named("Planned")), 1)
		})
	}
}

// sdk6017EventClient runs fn once, inside delivery of the first track event named event.
type sdk6017EventClient struct {
	*recorder
	event string
	fn    func()
	done  bool
}

func (c *sdk6017EventClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok && !c.done && e.Event == c.event {
		c.done = true
		c.fn()
	}
	return err
}

func TestSDK6017MergeThatNamesTheAgentOfACall(t *testing.T) {
	timed := func(startMS int64, span bool, agentName string) LLMCall {
		call := LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", InputTokens: 10, AgentName: agentName, StartedAt: time.UnixMilli(startMS), EndedAt: time.UnixMilli(startMS + 100)}
		if span {
			call.Integration = "tracing"
		}
		return call
	}
	for _, restored := range []bool{false, true} {
		where := map[bool]string{false: "first segment", true: "after a JSON restore"}[restored]
		// started is a fresh run, or one restored from the snapshot of an empty first segment.
		started := func(t *testing.T, client analytics.Client) *Interaction {
			t.Helper()
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			if !restored {
				return i
			}
			snapshot, ok := i.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause")
			}
			var stored PausedInteraction
			if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
				t.Fatal(err)
			}
			_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
			return i
		}
		counts := func(r *recorder) []int {
			if restored {
				return sdk6017ToolCounts(r)[1:]
			}
			return sdk6017ToolCounts(r)
		}

		for _, order := range []string{"tool after", "tool before"} {
			t.Run(where+"/by response ID moves its stamp and its tools to that agent ("+order+")", func(t *testing.T) {
				r := setupAI(t, sdk6017Rules(map[string]string{"search": "Searched"}))
				i := started(t, r)
				first := sdk6017Call("", 10)
				first.ResponseID = "resp_1"
				i.RecordCall(first)
				if order == "tool before" {
					i.RecordToolCalls(ToolCall{ID: "s1", Name: "search"})
				}
				// A framework reports the same response again and knows the agent.
				repeat := sdk6017Call("researcher", 10)
				repeat.ResponseID = "resp_1"
				i.RecordCall(repeat)
				if order == "tool after" {
					i.RecordToolCalls(ToolCall{ID: "s1", Name: "search"})
				}
				i.RecordCall(sdk6017Call("researcher", 10))
				i.Pause(PauseOptions{})
				// The researcher moved on: the pause does not wait on its tool.
				sdk6017Expect(t, "tool_call_count", counts(r), []int{1})
				sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"s1", "unknown"}})
			})
		}

		t.Run(where+"/leaves a tool of an earlier agentless call waiting on that call", func(t *testing.T) {
			r := setupAI(t)
			i := started(t, r)
			i.RecordCall(timed(1000, false, ""))
			i.RecordToolCalls(ToolCall{ID: "s1", Name: "search"})
			i.RecordCall(timed(3000, false, ""))
			// The second call turns out to be the planner's: no agentless call follows the tool.
			i.RecordCall(timed(3000, true, "planner"))
			i.Pause(PauseOptions{})
			sdk6017Expect(t, "tool_call_count", counts(r), []int{0})
			i.RecordToolResult("s1", ToolResult{Error: Ptr(false)})
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", counts(r), []int{0, 1})
		})

		t.Run(where+"/recorded while a pause settles its tool events ends the wait before the totals", func(t *testing.T) {
			r := setupAI(t, sdk6017Rules(map[string]string{"search": "Searched", "lookup": "Looked Up"}))
			client := &sdk6017EventClient{recorder: r, event: "Looked Up"}
			i := started(t, client)
			i.RecordToolCalls(ToolCall{ID: "s1", Name: "search", AgentName: "researcher"})
			i.RecordCall(timed(1000, false, ""))
			i.RecordToolCalls(ToolCall{Name: "lookup"})
			// The span twin names the researcher for the call after its tool, in the last callback.
			client.fn = func() { i.RecordCall(timed(1000, true, "researcher")) }
			i.Pause(PauseOptions{})
			sdk6017Expect(t, "tool_call_count", counts(r), []int{2})
			sdk6017Expect(t, "Searched", sdk6017ToolEvents(r, "Searched"), [][2]any{{"s1", "unknown"}})
		})
	}
}

func TestSDK6017ToolRecordedPastTheCallCap(t *testing.T) {
	ok := ToolCall{ID: "wait", Name: "approve_me", Result: "ok", Error: Ptr(false)}
	for _, restore := range []bool{false, true} {
		mode := map[bool]string{false: "in-process", true: "json"}[restore]
		capped := func(t *testing.T) *recorder {
			return setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: 1}), WithToolBreakdown(true), WithOnError(func(error, ErrorInfo) {}),
				sdk6017Rules(map[string]string{"approve_me": "Approved", "lookup": "Looked Up"}))
		}
		// One settled tool fills the cap; the next one is recorded past it.
		started := func(client analytics.Client) *Interaction {
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			i.RecordCall(sdk6017Call("", 10))
			i.RecordToolCalls(ToolCall{ID: "done", Name: "lookup", Result: "ok", Error: Ptr(false)})
			return i
		}

		t.Run(mode+"/is carried when the pause waits on it, and counted once when it runs", func(t *testing.T) {
			r := capped(t)
			first := started(r)
			first.RecordToolCalls(ToolCall{ID: "wait", Name: "approve_me"})
			i := sdk6017Continue(t, first, restore)
			i.RecordToolCalls(ok)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{1, 1})
			// Carried into a segment with room: an ordinary tool there, with its event.
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"wait", "success"}})
		})

		t.Run(mode+"/is counted at pause, without an event, when its agent made a later call", func(t *testing.T) {
			r := capped(t)
			first := started(r)
			first.RecordToolCalls(ToolCall{ID: "wait", Name: "approve_me"})
			first.RecordCall(sdk6017Call("", 10))
			i := sdk6017Continue(t, first, restore)
			i.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{2, 0})
			sdk6017Expect(t, "Approved", len(r.named("Approved")), 0)
		})

		for _, ran := range []string{"record", "span", "result", "duration"} {
			t.Run(mode+"/is counted once when it runs in the segment that recorded it ("+ran+")", func(t *testing.T) {
				r := capped(t)
				first := started(r)
				first.RecordToolCalls(ToolCall{ID: "wait", Name: "approve_me"})
				switch ran {
				case "record":
					first.RecordToolCalls(ok)
				case "span":
					first.recordToolCalls(true, ToolCall{Name: "approve_me", Error: Ptr(false)})
				case "result":
					first.RecordToolResult("wait", ToolResult{Result: "ok", Error: Ptr(false)})
				default:
					first.RecordToolResult("wait", ToolResult{Duration: Ptr(5 * time.Millisecond)})
				}
				i := sdk6017Continue(t, first, restore)
				i.End(EndOptions{})
				sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{2, 0})
				// Past the cap only its identity is counted: no outcome, no event.
				breakdown := pauseSegmentsJSON(t, r.named("ai_response_received")[0].Properties["tool_breakdown"]).([]any)
				found := false
				for _, entry := range breakdown {
					found = found || reflect.DeepEqual(entry, map[string]any{"tool_name": "approve_me", "call_count": 1.0})
				}
				if len(breakdown) != 2 || !found {
					t.Fatalf("tool_breakdown = %v, want lookup and an approve_me entry without an outcome", breakdown)
				}
				sdk6017Expect(t, "Approved", len(r.named("Approved")), 0)
			})
		}

		t.Run(mode+"/recorded during paused delivery stays past the cap: no event, carried by the next pause", func(t *testing.T) {
			r := capped(t)
			client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
			_, first := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			first.RecordCall(sdk6017Call("", 10))
			client.reenter = func() {
				first.RecordToolCalls(ToolCall{ID: "kept", Name: "lookup"}, ToolCall{ID: "wait", Name: "approve_me"})
			}
			second := sdk6017Continue(t, first, restore)
			second.RecordToolResult("kept", ToolResult{Error: Ptr(false)})
			third := sdk6017Continue(t, second, restore)
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1})
			third.RecordToolResult("wait", ToolResult{Error: Ptr(false)})
			third.End(EndOptions{})
			sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{0, 1, 1})
			sdk6017Expect(t, "Looked Up", sdk6017ToolEvents(r, "Looked Up"), [][2]any{{"kept", "success"}})
			sdk6017Expect(t, "Approved", sdk6017ToolEvents(r, "Approved"), [][2]any{{"wait", "success"}})
		})
	}

	t.Run("is never counted when the run ends while paused", func(t *testing.T) {
		r := setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: 1}), WithOnError(func(error, ErrorInfo) {}))
		i := sdk6017Start()
		i.RecordCall(sdk6017Call("", 10))
		i.RecordToolCalls(ToolCall{ID: "done", Name: "lookup", Error: Ptr(false)})
		i.RecordToolCalls(ToolCall{ID: "wait", Name: "approve_me"})
		i.Pause(PauseOptions{})
		i.End(EndOptions{})
		sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{1, 0})
	})

	t.Run("counts as unknown at the end of the segment that recorded it", func(t *testing.T) {
		r := setupAI(t, WithLimits(Limits{MaxCallsPerInteraction: 1}), WithOnError(func(error, ErrorInfo) {}), sdk6017Rules(map[string]string{"approve_me": "Approved"}))
		i := sdk6017Start()
		i.RecordCall(sdk6017Call("", 10))
		i.RecordToolCalls(ToolCall{ID: "done", Name: "lookup", Error: Ptr(false)})
		i.RecordToolCalls(ToolCall{ID: "wait", Name: "approve_me"})
		i.End(EndOptions{})
		sdk6017Expect(t, "tool_call_count", sdk6017ToolCounts(r), []int{2})
		sdk6017Expect(t, "Approved", len(r.named("Approved")), 0)
	})
}
