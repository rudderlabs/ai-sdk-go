package rudderstackai

import (
	"context"
	"encoding/json"
	"testing"
)

// A reused ID joins the most recently started open run, whether the older one paused and resumed
// in process, was restored from JSON, or never paused.
func TestJoinReusedIDPicksNewestStartedRun(t *testing.T) {
	for _, mode := range []string{"none", "resume", "restore"} {
		t.Run(mode, func(t *testing.T) {
			setupAI(t)
			opts := func(user string) InteractionOptions {
				return InteractionOptions{InteractionID: "x", UserID: user, NoPromptEvent: true, Classify: Ptr(false)}
			}
			_, a := StartInteraction(context.Background(), opts("tenant-a"))
			_, b := StartInteraction(context.Background(), opts("tenant-b"))
			target := a
			if mode != "none" {
				snap, ok := a.Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause refused")
				}
				if mode == "resume" {
					a.Resume()
				} else {
					var st PausedInteraction
					if err := json.Unmarshal([]byte(mustJSON(snap)), &st); err != nil {
						t.Fatal(err)
					}
					_, target = StartInteraction(context.Background(), InteractionOptions{Resume: &st, Classify: Ptr(false)})
				}
			}
			if got := FromContext(JoinInteraction(context.Background(), "x", JoinOptions{})); got != b {
				t.Errorf("joined %q, want tenant-b (started later)", got.userID)
			}
			target.End(EndOptions{})
			b.End(EndOptions{})
		})
	}
}

// Pausing and resuming one run registers its paused-slot cleanup once.
func TestPauseCleanupRegisteredOncePerInteraction(t *testing.T) {
	setupAI(t)
	n := 0
	onPauseCleanup = func() { n++ }
	t.Cleanup(func() { onPauseCleanup = nil })
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	for c := 0; c < 2000; c++ {
		if _, ok := i.Pause(PauseOptions{}); !ok {
			t.Fatalf("pause %d refused", c)
		}
		i.Resume()
	}
	if n != 1 {
		t.Errorf("%d cleanups registered after 2000 cycles, want 1", n)
	}
	i.End(EndOptions{})
}
