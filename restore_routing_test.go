package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestRestorePausedObjectsWithReusedID(t *testing.T) {
	for _, order := range [][]int{{0}, {1}, {0, 1}, {1, 0}} {
		for _, joined := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/joined=%v", order, joined), func(t *testing.T) {
				setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}), WithEmitSpans(true))
				var saved [2]context.Context
				var original, target [2]*Interaction
				var snapshots [2]PausedInteraction
				for n, user := range []string{"tenant-a", "tenant-b"} {
					saved[n], original[n] = StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: user, ConversationID: user + "-conv", Classify: Ptr(false)})
					if joined {
						saved[n] = JoinInteraction(context.Background(), "shared", JoinOptions{})
					}
					target[n] = original[n]
				}
				for n := range original {
					snap, ok := original[n].Pause(PauseOptions{})
					if !ok {
						t.Fatal("pause failed")
					}
					raw, err := json.Marshal(snap)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &snapshots[n]); err != nil {
						t.Fatal(err)
					}
				}
				_, refused := StartInteraction(context.Background(), InteractionOptions{UserID: "third"})
				if !refused.Ended() {
					t.Error("colliding paused objects did not each retain a slot")
					refused.End(EndOptions{})
				}
				for _, n := range order {
					_, target[n] = StartInteraction(context.Background(), InteractionOptions{Resume: &snapshots[n], Classify: Ptr(false)})
					if target[n].Ended() {
						t.Error("restore did not take its own paused slot")
					}
				}
				for n := range saved {
					if got := FromContext(saved[n]); got != target[n] {
						t.Errorf("tenant %d saved context resolves to %p, want %p", n, got, target[n])
					}
				}
				for n := range target {
					target[n].End(EndOptions{})
					original[n].End(EndOptions{})
				}
			})
		}
	}
}

func TestRestoreJSONInAnotherProcessDoesNotLinkReusedID(t *testing.T) {
	if raw := os.Getenv("RESTORE_ROUTING_SNAPSHOT"); raw != "" {
		setupAI(t, WithLimits(Limits{MaxOpenInteractions: 1}))
		saved, local := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: "tenant-b"})
		if _, ok := local.Pause(PauseOptions{}); !ok {
			t.Fatal("pause failed")
		}
		var snapshot PausedInteraction
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			t.Fatal(err)
		}
		_, refused := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
		if !refused.Ended() || FromContext(saved) != local {
			t.Fatal("foreign JSON restore stole the local paused slot or context")
		}
		local.End(EndOptions{})
		_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
		if restored.Ended() || restored.userID != "tenant-a" {
			t.Fatal("foreign JSON restore failed normal admission")
		}
		restored.End(EndOptions{})
		return
	}
	setupAI(t)
	_, source := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: "tenant-a"})
	snapshot, ok := source.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause failed")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	source.End(EndOptions{})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestRestoreJSONInAnotherProcessDoesNotLinkReusedID$")
	cmd.Env = append(os.Environ(), "RESTORE_ROUTING_SNAPSHOT="+string(raw))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestRestoreLegacyOrStaleSnapshotDoesNotLink(t *testing.T) {
	for _, kind := range []string{"legacy", "resumed", "paused-again", "consumed"} {
		t.Run(kind, func(t *testing.T) {
			setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
			saved, source := StartInteraction(context.Background(), InteractionOptions{InteractionID: "shared", UserID: "tenant-a"})
			snapshot, ok := source.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause failed")
			}
			owner := source
			switch kind {
			case "legacy":
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "pause_token")
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
			case "resumed":
				source.Resume()
			case "paused-again":
				source.Resume()
				if _, ok := source.Pause(PauseOptions{}); !ok {
					t.Fatal("second pause failed")
				}
			case "consumed":
				_, owner = StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
				if _, ok := owner.Pause(PauseOptions{}); !ok {
					t.Fatal("restored pause failed")
				}
			}
			_, copyRun := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
			if copyRun.Ended() {
				t.Error("unlinked snapshot failed admission with room")
			}
			if FromContext(saved) != owner {
				t.Error("snapshot without a matching current pause redirected the saved context")
			}
			copyRun.End(EndOptions{})
			owner.End(EndOptions{})
			source.End(EndOptions{})
		})
	}
}

// A snapshot key this version does not know (counted_tool_ids, written by a newer SDK) is
// ignored: the run restores with the same totals as without it.
func TestRestoreIgnoresUnknownSnapshotKeys(t *testing.T) {
	totals := func(t *testing.T, extra bool, inProcess bool) map[string]any {
		r := setupAI(t, WithLimits(Limits{MaxOpenInteractions: 2}))
		_, source := StartInteraction(context.Background(), InteractionOptions{InteractionID: "run", UserID: "u", Classify: Ptr(false)})
		source.RecordCall(pauseCall)
		snapshot, ok := source.Pause(PauseOptions{})
		if !ok {
			t.Fatal("pause failed")
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["pause_token"] == nil {
			t.Fatal("snapshot carries no pause_token")
		}
		if extra {
			fields["counted_tool_ids"] = json.RawMessage(`["call_1"]`)
		}
		if raw, err = json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
		var stored PausedInteraction
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !inProcess {
			source.End(EndOptions{})
		}
		_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Classify: Ptr(false)})
		if restored.Ended() {
			t.Fatal("restore was refused")
		}
		restored.RecordCall(pauseCall)
		restored.End(EndOptions{})
		source.End(EndOptions{})
		events := r.named("ai_response_received")
		if len(events) == 0 {
			t.Fatal("no response event")
		}
		last := events[len(events)-1].Properties
		return map[string]any{"calls": last["llm_call_count"], "input": last["total_input_tokens"], "output": last["total_output_tokens"], "tools": last["tool_call_count"]}
	}
	for _, inProcess := range []bool{false, true} {
		t.Run(fmt.Sprintf("in-process=%v", inProcess), func(t *testing.T) {
			without := totals(t, false, inProcess)
			with := totals(t, true, inProcess)
			if fmt.Sprint(with) != fmt.Sprint(without) {
				t.Errorf("totals with counted_tool_ids = %v, without = %v", with, without)
			}
			if without["calls"] == nil {
				t.Errorf("no totals read: %v", without)
			}
		})
	}
}
