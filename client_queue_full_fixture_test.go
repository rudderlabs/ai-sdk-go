package rudderstackai

// Shared fixture: events sent while the client's queue is full. analytics-go blocks, never drops.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type queueFullExpected struct {
	Delivered []string `json:"delivered"`
	Errors    []string `json:"errors"`
}

type queueFullFixture struct {
	Cases []struct {
		Name       string            `json:"name"`
		Path       string            `json:"path"`
		Capacity   int               `json:"capacity"`
		ExpectedGo queueFullExpected `json:"expected_go"`
	} `json:"cases"`
}

func TestSharedClientQueueFullFixture(t *testing.T) {
	var f queueFullFixture
	loadFixture(t, "spec/fixtures/client-queue-full-v0.1.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	// The same events each path sends, so Go's expectation cannot shrink to match a drop.
	sent := map[string][]string{
		"interaction": {"ai_user_prompt_created", "ai_response_received"},
		"span":        {"ai_otel_span"},
		"tracing":     {"ai_otel_span"},
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			want, known := sent[c.Path]
			if !known {
				t.Fatalf("unknown path %q", c.Path)
			}
			if !slices.Equal(c.ExpectedGo.Delivered, want) || len(c.ExpectedGo.Errors) != 0 {
				t.Fatalf("expected_go must deliver every event and report no errors: %+v", c.ExpectedGo)
			}
			if c.Path == "tracing" {
				return // runs in rudderotel, which owns the exporter
			}
			q := testkit.NewQueueFull(c.Capacity)
			setupAI(t) // resets state; the queue client below replaces the recorder
			var mu sync.Mutex
			var errs []string
			if err := Configure(WithAnalytics(q), WithEmitSpans(c.Path == "span"),
				WithOnError(func(_ error, info ErrorInfo) {
					mu.Lock()
					defer mu.Unlock()
					errs = append(errs, info.Kind)
				})); err != nil {
				t.Fatal(err)
			}
			path := func() {
				ctx := context.Background()
				switch c.Path {
				case "interaction":
					_, i := StartInteraction(ctx, InteractionOptions{UserID: "u", PromptText: "hello", Classify: Ptr(false)})
					i.End(EndOptions{ResponseText: "answer"})
				case "span":
					_, s := StartSpan(ctx, SpanOptions{Name: "step", Operation: "execute_tool"})
					s.End(nil)
				}
				waitBackgroundWork(10 * time.Second)
			}
			got := q.Run(t, len(want), path)
			if !slices.Equal(got, c.ExpectedGo.Delivered) {
				t.Errorf("delivered = %v, want %v", got, c.ExpectedGo.Delivered)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(errs) != 0 {
				t.Errorf("errors = %v, want none", errs)
			}
		})
	}
}
