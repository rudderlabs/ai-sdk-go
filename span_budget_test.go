package rudderstackai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The size check for replacing or covering an OTel span must agree with delivery at the byte:
// a span fits exactly when the same span, delivered, is sent.
func TestSpanSizeCheckMatchesDeliveryAtTheBudget(t *testing.T) {
	started := time.Date(2026, 11, 17, 1, 2, 3, 0, time.UTC)
	ended := started.Add(1500 * time.Millisecond)
	// deliver runs one wrapper span whose message context holds pad bytes and reports the check
	// and the delivered size (0 when delivery dropped the span).
	deliver := func(t *testing.T, pad int) (fits bool, size int) {
		t.Helper()
		r := setupAI(t, WithEmitSpans(true))
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", pad)}})
		_, s := beginWrapperSpan(ctx, i, started)
		fits = s.fitsAt(ended, s.TraceID, s.SpanID, s.parentID)
		s.finish("chat", "chat", "wrapper", nil, nil, false, ended)
		for _, m := range r.named("ai_otel_span") {
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			size = len(b)
		}
		return fits, size
	}
	_, base := deliver(t, 0)
	if base == 0 {
		t.Fatal("an empty context was not delivered")
	}
	// Message size grows one byte per padding byte, so this pad puts it exactly at the budget.
	budgetPad := 32_000 - base
	for _, c := range []struct {
		pad  int
		want bool
	}{{budgetPad - 1, true}, {budgetPad, true}, {budgetPad + 1, false}} {
		fits, size := deliver(t, c.pad)
		if fits != c.want || (size > 0) != c.want {
			t.Errorf("pad %d (budget%+d): check=%v delivered=%v, want both %v", c.pad, c.pad-budgetPad, fits, size > 0, c.want)
		}
		if c.pad == budgetPad && size != 32_000 {
			t.Errorf("pad %d delivered %d bytes, want exactly 32000", c.pad, size)
		}
	}
}

// A context near the budget used to pass a context-plus-identity check and then lose the span
// at delivery: the check measures the whole message.
func TestSpanSizeCheckCountsTheWholeMessage(t *testing.T) {
	for _, pad := range []int{31_000, 31_500} {
		setupAI(t, WithEmitSpans(true))
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", pad)}})
		_, s := beginWrapperSpan(ctx, i, time.Now())
		if s.canSend() {
			t.Errorf("a %d-byte context passed the check", pad)
		}
	}
	setupAI(t, WithEmitSpans(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", 1_000)}})
	_, s := beginWrapperSpan(ctx, i, time.Now())
	if !s.canSend() {
		t.Error("a span with room failed the check")
	}
}
