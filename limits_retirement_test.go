package rudderstackai

import (
	"context"
	"testing"
)

func TestSDK7017RetirementFIFOAfterLimitChanges(t *testing.T) {
	setupAI(t, WithLimits(Limits{MaxMapEntries: 3}), WithOnError(func(error, ErrorInfo) {}))
	end := func(id string) {
		_, i := StartInteraction(context.Background(), InteractionOptions{InteractionID: id, NoPromptEvent: true, Classify: Ptr(false)})
		i.End(EndOptions{})
	}
	check := func(ids ...string) {
		t.Helper()
		open.mu.Lock()
		defer open.mu.Unlock()
		if len(open.retired) != len(ids) {
			t.Fatalf("retained %d IDs, want %v", len(open.retired), ids)
		}
		for _, id := range ids {
			if open.retired[id] == nil {
				t.Fatalf("missing retained ID %q", id)
			}
		}
	}
	end("a")
	end("b")
	end("c")
	end("a") // The latest retirement moves a to the back, ahead of the next eviction.
	end("d")
	check("c", "a", "d")
	if err := Configure(WithLimits(Limits{MaxMapEntries: 1})); err != nil {
		t.Fatal(err)
	}
	end("a") // Shrinking still keeps the ID being updated, even when it was already present.
	check("a")
	end("a") // Moving the only entry must not lose it or count as an eviction.
	check("a")
	end("e")
	check("e")
	if err := Configure(WithLimits(Limits{MaxMapEntries: 3})); err != nil {
		t.Fatal(err)
	}
	end("f")
	end("g")
	end("h")
	check("f", "g", "h")
	open.mu.Lock()
	defer open.mu.Unlock()
	if !retiredSinceLocked("unknown", 0) || retiredSinceLocked("unknown", open.nextOrder) {
		t.Fatal("eviction floor must fail closed only for spans predating the eviction")
	}
}
