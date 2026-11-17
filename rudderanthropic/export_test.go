package rudderanthropic

import (
	"context"
	"reflect"
	"slices"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// ResetManagedAgents forgets every session's read events, threads and paused turns (tests only).
func ResetManagedAgents() {
	borrowedTurns.Lock()
	borrowedTurns.m, borrowedTurns.order = map[string]*borrowedTurn{}, nil
	borrowedTurns.Unlock()
	seenEvents.clear()
	childThreads.clear()
	childTools.clear()
	pausedTurns.Lock()
	pausedTurns.m, pausedTurns.order = map[string]*pausedTurn{}, nil
	pausedTurns.Unlock()
}

// PanickingOption is a session option that panics, like a getter that throws (tests only).
func PanickingOption() SessionOption { return func(*sessionConfig) { panic("option failed") } }

// ManagedAgentsTurnType exposes the real opened turn's type for reflection-based field coverage.
func ManagedAgentsTurnType() reflect.Type {
	tracker := &sessionTracker{ctx: context.Background()}
	tracker.observe(`{"type":"user.message"}`)
	defer tracker.close(nil)
	return reflect.TypeOf(*tracker.turn)
}

func checkManagedAgentsMap[V any](t *testing.T, b *boundedMap[V], value V) {
	t.Helper()
	keys := func() []string {
		var out []string
		for _, entry := range b.entries() {
			out = append(out, entry.key)
		}
		return out
	}
	for _, key := range []string{"oldest", "middle", "newest"} {
		b.set(key, value)
	}
	if got := keys(); !slices.Equal(got, []string{"middle", "newest"}) {
		t.Fatalf("%s: %v", b.where, got)
	}
	// Updating preserves insertion order; deleting and reinserting makes a key newest.
	b.set("middle", value)
	b.delete("middle")
	b.set("middle", value)
	b.set("last", value)
	if got := keys(); !slices.Equal(got, []string{"middle", "last"}) {
		t.Fatalf("%s after reinsertion: %v", b.where, got)
	}
	b.clear()
}

// CheckManagedAgentsBounds checks all carried maps and the paused and borrowed session maps.
// The caller configures MaxMapEntries to two before invoking it.
func CheckManagedAgentsBounds(t *testing.T) {
	t.Helper()
	tracker := &sessionTracker{ctx: context.Background(), cfg: sessionConfig{sessionID: "inspect"}}
	tracker.observe(`{"type":"user.message"}`)
	state := tracker.turn.state
	checkManagedAgentsMap(t, state.starts, requestStart{})
	checkManagedAgentsMap(t, state.recorded, msTime{})
	checkManagedAgentsMap(t, state.blocking, rudderstackai.PauseApproval)
	checkManagedAgentsMap(t, state.tools, (*turnTool)(nil))
	tracker.close(nil)
	ResetManagedAgents()
	for _, borrowed := range []bool{false, true} {
		ctx := context.Background()
		var interaction *rudderstackai.Interaction
		if borrowed {
			ctx, interaction = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{NoPromptEvent: true})
		}
		for _, id := range []string{"oldest", "middle", "newest"} {
			next := &sessionTracker{ctx: ctx, cfg: sessionConfig{sessionID: id}}
			next.observe(`{"type":"user.message"}`)
			next.observe(`{"type":"session.status_idle","stop_reason":{"type":"requires_action"}}`)
			carried := next.turn.state
			next.close(nil)
			if borrowed {
				borrowedTurns.Lock()
				same := borrowedTurns.m[id] != nil && borrowedTurns.m[id].state == carried
				borrowedTurns.Unlock()
				if !same {
					t.Fatal("borrowed turn did not carry whole state")
				}
			} else {
				pausedTurns.Lock()
				same := pausedTurns.m[id] != nil && pausedTurns.m[id].state == carried
				pausedTurns.Unlock()
				if !same {
					t.Fatal("paused turn did not carry whole state")
				}
			}
		}
		var keys []string
		if borrowed {
			borrowedTurns.Lock()
			keys = slices.Clone(borrowedTurns.order)
			borrowedTurns.Unlock()
		} else {
			pausedTurns.Lock()
			keys = slices.Clone(pausedTurns.order)
			pausedTurns.Unlock()
		}
		if !slices.Equal(keys, []string{"middle", "newest"}) {
			t.Fatalf("borrowed=%v: %v", borrowed, keys)
		}
		// Adoption/resume restores the original state object, without reconstructing its fields.
		next := &sessionTracker{ctx: ctx, cfg: sessionConfig{sessionID: "newest"}}
		var carried *turnState
		if borrowed {
			borrowedTurns.Lock()
			carried = borrowedTurns.m["newest"].state
			borrowedTurns.Unlock()
		} else {
			pausedTurns.Lock()
			carried = pausedTurns.m["newest"].state
			pausedTurns.Unlock()
		}
		next.observe(`{"type":"agent.thinking"}`)
		if next.turn.state != carried {
			t.Fatal("resume/adoption did not restore whole state")
		}
		next.close(nil)
		if interaction != nil {
			interaction.End(rudderstackai.EndOptions{})
		}
		ResetManagedAgents()
	}
}
