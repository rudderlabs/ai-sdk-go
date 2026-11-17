package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
	"weak"
)

// pauseGCProbe is deliberately larger than the runtime's tiny allocation size.
type pauseGCProbe struct{ data [64]byte }

func requireCollected[T any](t *testing.T, p weak.Pointer[T]) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.Value() != nil {
		if time.Now().After(deadline) {
			t.Fatal("SDK retained the dropped paused object")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

func TestPausedSnapshotDoesNotRetainInteraction(t *testing.T) {
	if raw := os.Getenv("PAUSE_GC_SNAPSHOT"); raw != "" {
		setupAI(t, WithLimits(Limits{MaxOpenInteractions: 1}))
		var snapshot PausedInteraction
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			t.Fatal(err)
		}
		_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot, Classify: Ptr(false)})
		if restored.Ended() || restored.userID != "tenant-a" || openCount() != 1 {
			t.Fatal("other worker could not restore the collected interaction's JSON")
		}
		restored.End(EndOptions{})
		_, next := StartInteraction(context.Background(), InteractionOptions{UserID: "tenant-b"})
		if next.Ended() {
			t.Fatal("other worker's restored end did not free its slot")
		}
		next.End(EndOptions{})
		return
	}
	for _, mode := range []string{"snapshot", "json", "restored", "another-process"} {
		t.Run(mode, func(t *testing.T) {
			setupAI(t, WithLimits(Limits{MaxOpenInteractions: 1}))
			snapshot, object, scope := func() (PausedInteraction, weak.Pointer[Interaction], weak.Pointer[pauseGCProbe]) {
				probe := &pauseGCProbe{}
				ctx := context.WithValue(context.Background(), keyValues, &contextValues{disconnected: func() bool {
					runtime.KeepAlive(probe)
					return false
				}})
				_, i := StartInteraction(ctx, InteractionOptions{InteractionID: "shared", UserID: "tenant-a", Classify: Ptr(false)})
				snapshot, ok := i.Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause failed")
				}
				if mode == "restored" {
					_, next := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot, Classify: Ptr(false)})
					if FromContext(context.WithValue(ctx, keyInteraction, i)) != next {
						t.Fatal("saved context did not follow restore")
					}
					i = next
					snapshot, ok = i.Pause(PauseOptions{})
					if !ok {
						t.Fatal("restored pause failed")
					}
				}
				return snapshot, weak.Make(i), weak.Make(probe)
			}()
			if mode == "json" {
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
			}
			requireCollected(t, object)
			requireCollected(t, scope)
			if mode == "another-process" {
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(executable, "-test.run=^TestPausedSnapshotDoesNotRetainInteraction$")
				cmd.Env = append(os.Environ(), "PAUSE_GC_SNAPSHOT="+string(raw))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("child: %v\n%s", err, out)
				}
			}
			_, refused := StartInteraction(context.Background(), InteractionOptions{UserID: "tenant-b"})
			if !refused.Ended() {
				t.Fatal("GC freed the paused slot")
			}
			foreign := snapshot
			foreign.InteractionID = "other"
			_, refused = StartInteraction(context.Background(), InteractionOptions{Resume: &foreign})
			if !refused.Ended() {
				t.Fatal("mismatched ID took the collected owner's slot")
			}
			_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot, Classify: Ptr(false)})
			if restored.Ended() || restored.userID != "tenant-a" || openCount() != 1 {
				t.Fatal("snapshot could not restore the collected owner's slot")
			}
			restored.End(EndOptions{})
			_, next := StartInteraction(context.Background(), InteractionOptions{UserID: "tenant-b"})
			if next.Ended() {
				t.Fatal("restored end did not free the slot")
			}
			next.End(EndOptions{})
		})
	}
}
