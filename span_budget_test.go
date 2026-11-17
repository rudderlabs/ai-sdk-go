package rudderstackai

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

// Replacing or covering an OTel span is admitted when the span's mandatory core plus the
// completion reserve fits the budget, at the byte (FIXTURE-FORMAT.md, SDK span delivery budget).
func TestSpanAdmissionFitsTheCoreWithTheCompletionReserve(t *testing.T) {
	started := time.Date(2026, 11, 17, 1, 2, 3, 0, time.UTC)
	ended := started.Add(1500 * time.Millisecond)
	// core reports whether a wrapper span whose message context holds pad bytes is admitted, and
	// its core's size.
	core := func(t *testing.T, pad int) (bool, int) {
		t.Helper()
		setupAI(t, WithEmitSpans(true))
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", pad)}})
		_, s := beginWrapperSpan(ctx, i, started)
		c, ok := s.admitAt(ended, s.TraceID, s.SpanID, s.parentID)
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return ok, len(b)
	}
	_, base := core(t, 0)
	// The core grows one byte per padding byte (context is mandatory).
	budgetPad := 32_000 - spanmodel.CompletionReserveBytes - base
	for _, c := range []struct {
		pad  int
		want bool
	}{{budgetPad - 1, true}, {budgetPad, true}, {budgetPad + 1, false}} {
		if ok, _ := core(t, c.pad); ok != c.want {
			t.Errorf("pad %d (budget%+d): admitted=%v, want %v", c.pad, c.pad-budgetPad, ok, c.want)
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

// A wrapper span whose mandatory message context cannot be encoded can never be sent, so it is
// never admitted to replace or cover an OTel span, however small.
func TestSpanAdmissionRefusesAnUnencodableContext(t *testing.T) {
	for name, value := range map[string]any{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
		"invalid json.Number in a map":  map[string]any{"n": json.Number("not-a-number")},
		"invalid json.Number in a list": []any{1, json.Number("1e")},
	} {
		t.Run(name, func(t *testing.T) {
			setupAI(t, WithEmitSpans(true))
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"bad": value}})
			_, s := beginWrapperSpan(ctx, i, time.Now())
			if s.canSend() {
				t.Error("an unencodable context passed the check")
			}
			if _, ok := s.admitAt(time.Now(), s.TraceID, "1111111111111111", s.SpanID); ok {
				t.Error("an unencodable context was admitted under adopted IDs")
			}
		})
	}
}
