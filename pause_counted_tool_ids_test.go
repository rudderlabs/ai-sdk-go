package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// SDK-7226: a tool call an earlier segment counted (for example as unknown) and that is reported
// again after the resume counts once.

func countedToolSetup(t *testing.T, opts ...ConfigOption) *recorder {
	t.Helper()
	return setupAI(t, append([]ConfigOption{WithToolBreakdown(true),
		WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_done"}}})}, opts...)...)
}

func lookupEventsByID(r *recorder, id string) []map[string]any {
	var out []map[string]any
	for _, e := range r.named("lookup_done") {
		if e.Properties["tool_call_id"] == id {
			out = append(out, e.Properties)
		}
	}
	return out
}

func summedToolCalls(r *recorder, interactionID string) int {
	sum := 0
	for _, e := range r.named("ai_response_received") {
		if e.Properties["interaction_id"] != interactionID {
			continue
		}
		if n, ok := e.Properties["tool_call_count"].(int); ok {
			sum += n
		}
	}
	return sum
}

// pausedWithUnknownTool records tool id without a result, a later model call by the same agent,
// and pauses: the tool counts in the paused segment as unknown.
func pausedWithUnknownTool(t *testing.T, ids ...string) (*Interaction, PausedInteraction) {
	t.Helper()
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	for _, id := range ids {
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: id})
	}
	i.RecordCall(pauseCall)
	paused, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause refused")
	}
	return i, paused
}

func roundTrip(t *testing.T, paused PausedInteraction, edit func(map[string]any)) PausedInteraction {
	t.Helper()
	b, err := json.Marshal(paused)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	b, _ = json.Marshal(m)
	var stored PausedInteraction
	_ = json.Unmarshal(b, &stored) // a malformed field errors and marks the snapshot unusable
	return stored
}

// countedIDs reads the written counted_tool_ids through JSON, the portable form.
func countedIDs(t *testing.T, paused PausedInteraction) []string {
	t.Helper()
	var ids []string
	roundTrip(t, paused, func(m map[string]any) {
		vs, _ := m["counted_tool_ids"].([]any)
		for _, v := range vs {
			ids = append(ids, v.(string))
		}
	})
	return ids
}

func TestToolReportedAgainAfterPauseCountsOnce(t *testing.T) {
	again := func(i *Interaction) {
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: "t1", Result: "x", Error: Ptr(false)})
	}
	t.Run("record after resume is ignored", func(t *testing.T) {
		r := countedToolSetup(t)
		i, _ := pausedWithUnknownTool(t, "t1")
		i.Resume()
		again(i)
		i.End(EndOptions{})
		events := lookupEventsByID(r, "t1")
		if len(events) != 1 || events[0]["tool_status"] != "unknown" || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", events, summedToolCalls(r, i.ID()))
		}
	})
	t.Run("record while paused is ignored and does not resume", func(t *testing.T) {
		r := countedToolSetup(t)
		i, _ := pausedWithUnknownTool(t, "t1")
		again(i)
		if !i.Paused() {
			t.Fatal("an ignored report resumed the run")
		}
		i.Resume()
		i.End(EndOptions{})
		if events := lookupEventsByID(r, "t1"); len(events) != 1 || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", events, summedToolCalls(r, i.ID()))
		}
	})
	t.Run("a multi-tool report skips only the counted ID", func(t *testing.T) {
		r := countedToolSetup(t)
		i, _ := pausedWithUnknownTool(t, "t1")
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: "t1", Result: "x", Error: Ptr(false)}, ToolCall{Name: "lookup", ID: "t2", Result: "y", Error: Ptr(false)})
		if i.Paused() {
			t.Fatal("the new tool call must resume the run")
		}
		i.End(EndOptions{})
		if len(lookupEventsByID(r, "t1")) != 1 || len(lookupEventsByID(r, "t2")) != 1 || summedToolCalls(r, i.ID()) != 2 {
			t.Fatalf("t1 %v t2 %v summed %d", lookupEventsByID(r, "t1"), lookupEventsByID(r, "t2"), summedToolCalls(r, i.ID()))
		}
	})
	t.Run("a tool span for the ID is ignored", func(t *testing.T) {
		r := countedToolSetup(t)
		i, _ := pausedWithUnknownTool(t, "t1")
		i.Resume()
		i.recordToolCalls(true, false, ToolCall{Name: "lookup", ID: "t1"})
		i.End(EndOptions{})
		if len(lookupEventsByID(r, "t1")) != 1 || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", lookupEventsByID(r, "t1"), summedToolCalls(r, i.ID()))
		}
	})
	t.Run("result-only report stays ignored", func(t *testing.T) {
		r := countedToolSetup(t)
		i, _ := pausedWithUnknownTool(t, "t1")
		i.Resume()
		i.RecordToolResult("t1", ToolResult{Result: "x"})
		i.End(EndOptions{})
		if len(lookupEventsByID(r, "t1")) != 1 || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", lookupEventsByID(r, "t1"), summedToolCalls(r, i.ID()))
		}
	})
	t.Run("without a pause the report merges", func(t *testing.T) {
		r := countedToolSetup(t)
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: "t1"})
		i.RecordCall(pauseCall)
		again(i)
		i.End(EndOptions{})
		events := lookupEventsByID(r, "t1")
		if len(events) != 1 || events[0]["tool_status"] != "success" || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", events, summedToolCalls(r, i.ID()))
		}
	})
	t.Run("a waiting tool still merges after the resume", func(t *testing.T) {
		r := countedToolSetup(t)
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
		i.RecordCall(pauseCall)
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: "t1"})
		i.Pause(PauseOptions{})
		i.Resume()
		again(i)
		i.End(EndOptions{})
		events := lookupEventsByID(r, "t1")
		if len(events) != 1 || events[0]["tool_status"] != "success" || summedToolCalls(r, i.ID()) != 1 {
			t.Fatalf("events %v, summed %d", events, summedToolCalls(r, i.ID()))
		}
	})
}

func TestCountedToolIDsInSnapshot(t *testing.T) {
	again := func(i *Interaction) {
		i.RecordToolCalls(ToolCall{Name: "lookup", ID: "t1", Result: "x", Error: Ptr(false)})
	}
	t.Run("JSON restore ignores the repeated ID", func(t *testing.T) {
		r := countedToolSetup(t)
		_, paused := pausedWithUnknownTool(t, "t1")
		stored := roundTrip(t, paused, func(m map[string]any) {
			if ids, _ := m["counted_tool_ids"].([]any); len(ids) != 1 || ids[0] != "t1" {
				t.Fatalf("counted_tool_ids: %v", m["counted_tool_ids"])
			}
		})
		_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		again(resumed)
		resumed.End(EndOptions{})
		if len(lookupEventsByID(r, "t1")) != 1 || summedToolCalls(r, resumed.ID()) != 1 {
			t.Fatalf("events %v, summed %d", lookupEventsByID(r, "t1"), summedToolCalls(r, resumed.ID()))
		}
	})
	t.Run("a snapshot without the field counts it again", func(t *testing.T) {
		r := countedToolSetup(t)
		_, paused := pausedWithUnknownTool(t, "t1")
		stored := roundTrip(t, paused, func(m map[string]any) { delete(m, "counted_tool_ids") })
		_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		again(resumed)
		resumed.End(EndOptions{})
		if len(lookupEventsByID(r, "t1")) != 2 || summedToolCalls(r, resumed.ID()) != 2 {
			t.Fatalf("events %v, summed %d", lookupEventsByID(r, "t1"), summedToolCalls(r, resumed.ID()))
		}
	})
	t.Run("the field is omitted when empty", func(t *testing.T) {
		countedToolSetup(t)
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
		i.RecordCall(pauseCall)
		paused, _ := i.Pause(PauseOptions{})
		roundTrip(t, paused, func(m map[string]any) {
			if _, present := m["counted_tool_ids"]; present {
				t.Fatalf("counted_tool_ids written when empty: %v", m["counted_tool_ids"])
			}
		})
	})
	t.Run("pausing again writes the same set", func(t *testing.T) {
		countedToolSetup(t)
		i, first := pausedWithUnknownTool(t, "t1")
		second, _ := i.Pause(PauseOptions{})
		if !slices.Equal(countedIDs(t, first), []string{"t1"}) || !slices.Equal(countedIDs(t, second), countedIDs(t, first)) {
			t.Fatalf("first %v, second %v", countedIDs(t, first), countedIDs(t, second))
		}
	})
	t.Run("a restored run snapshots the set again", func(t *testing.T) {
		countedToolSetup(t)
		_, paused := pausedWithUnknownTool(t, "t1")
		stored := roundTrip(t, paused, nil)
		_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		resumed.RecordCall(pauseCall)
		again, ok := resumed.Pause(PauseOptions{})
		if !ok || !slices.Equal(countedIDs(t, again), []string{"t1"}) {
			t.Fatalf("ok %v, ids %v", ok, countedIDs(t, again))
		}
	})
}

func TestCountedToolIDsAreBounded(t *testing.T) {
	record := func(i *Interaction, ids ...string) {
		for _, id := range ids {
			i.RecordToolCalls(ToolCall{Name: "lookup", ID: id, Result: "x"})
		}
	}
	t.Run("in process, the newest IDs are kept", func(t *testing.T) {
		r := countedToolSetup(t, WithLimits(Limits{MaxCallsPerInteraction: 2}))
		i, paused := pausedWithUnknownTool(t, "a", "b", "c")
		if !slices.Equal(countedIDs(t, paused), []string{"b", "c"}) {
			t.Fatalf("counted ids %v", countedIDs(t, paused))
		}
		i.Resume()
		record(i, "b", "c") // remembered: ignored
		record(i, "a")      // evicted: counts again
		i.End(EndOptions{})
		if n := summedToolCalls(r, i.ID()); n != 4 {
			t.Fatalf("summed tool_call_count %d, want 4", n)
		}
	})
	t.Run("after restore, the newest IDs are kept under the reader's limit", func(t *testing.T) {
		r := countedToolSetup(t, WithLimits(Limits{MaxCallsPerInteraction: 2}))
		_, paused := pausedWithUnknownTool(t, "x")
		stored := roundTrip(t, paused, func(m map[string]any) { m["counted_tool_ids"] = []string{"a", "b", "a", "c"} })
		_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		record(resumed, "b", "c")
		record(resumed, "a")
		resumed.End(EndOptions{})
		if n := summedToolCalls(r, resumed.ID()); n != 2 {
			t.Fatalf("summed tool_call_count %d, want 2 (x from the paused segment, plus the evicted a)", n)
		}
	})
}

func TestMalformedCountedToolIDsStartAFreshInteraction(t *testing.T) {
	for name, value := range map[string]any{
		"null":         nil,
		"string":       "t1",
		"object":       map[string]any{"t1": true},
		"empty string": []any{"t1", ""},
		"number":       []any{"t1", 7},
		"null item":    []any{nil},
	} {
		t.Run(name, func(t *testing.T) {
			countedToolSetup(t)
			logs := captureLogs(t)
			_, paused := pausedWithUnknownTool(t, "t1")
			stored := roundTrip(t, paused, func(m map[string]any) { m["counted_tool_ids"] = value })
			_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			if resumed.ID() == paused.InteractionID || logs.count("Resume:") == 0 {
				t.Fatalf("restored a malformed snapshot: id %s, logs %s", resumed.ID(), logs.String())
			}
			resumed.End(EndOptions{})
		})
	}
}

// Workers report tool calls while a driver pauses and resumes the run: each ID counts once.
func TestToolReportsRacingPauseCountOnce(t *testing.T) {
	const workers, rounds = 40, 20
	for round := range rounds {
		r := countedToolSetup(t)
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
		done := make(chan struct{})
		var driver, wg sync.WaitGroup
		driver.Add(1)
		go func() {
			defer driver.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				i.RecordCall(pauseCall)
				if _, ok := i.Pause(PauseOptions{}); ok {
					i.Resume()
				}
			}
		}()
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("t%d", w)
				i.RecordToolCalls(ToolCall{Name: "lookup", ID: id})
				i.RecordToolCalls(ToolCall{Name: "lookup", ID: id, Result: "x"})
			}()
		}
		wg.Wait()
		close(done)
		driver.Wait()
		i.End(EndOptions{})
		for w := range workers {
			if n := len(lookupEventsByID(r, fmt.Sprintf("t%d", w))); n != 1 {
				t.Fatalf("round %d: tool t%d sent %d declared events, want 1", round, w, n)
			}
		}
		if n := summedToolCalls(r, i.ID()); n != workers {
			t.Fatalf("round %d: summed tool_call_count %d, want %d", round, n, workers)
		}
	}
}
