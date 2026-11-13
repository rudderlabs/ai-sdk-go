package rudderstackai

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestSpanRootBufferOmitted(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	_, s := StartSpan(context.Background(), SpanOptions{Attributes: map[string]any{"gen_ai.tool.call.arguments": map[string]any{"type": "Buffer", "data": []int{1, 2}}}})
	s.End(nil)
	if _, ok := spanAttributes(r.named("ai_otel_span")[0].Properties)["gen_ai.tool.call.arguments"]; ok {
		t.Fatal("root Buffer leaked")
	}
}

func TestSpanDateString(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	date := time.Date(2026, 11, 13, 0, 0, 0, 123000000, time.UTC)
	_, s := StartSpan(context.Background(), SpanOptions{Attributes: map[string]any{"gen_ai.tool.call.arguments": map[string]any{"date": &date}}})
	s.End(nil)
	got := spanAttributes(r.named("ai_otel_span")[0].Properties)["gen_ai.tool.call.arguments"]
	if !reflect.DeepEqual(got, map[string]any{"date": "2026-11-13T00:00:00.123Z"}) {
		t.Fatalf("date=%v", got)
	}
}

func TestWalkerContainerDepthBoundary(t *testing.T) {
	// As on main, each map and its interface-valued child count as separate hops.
	for _, depth := range []int{31, 32, 33, 40, 64, 65} {
		var v any = "leaf"
		for range depth {
			v = map[string]any{"child": v}
		}
		got := stripBinary(v)
		for n := 0; n < min(depth, 32); n++ {
			m, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("depth %d truncated at %d: %v", depth, n, got)
			}
			got = m["child"]
		}
		if depth < 32 && got != "leaf" {
			t.Fatalf("depth %d lost leaf: %v", depth, got)
		}
		if depth >= 32 && got != nil {
			t.Fatalf("depth limit not applied: %v", got)
		}
	}
}

func TestWalkerCyclicListEdgesRemoved(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	a := []any{"keep", nil, func() {}}
	a[1] = a
	got := stripBinary(a)
	if !reflect.DeepEqual(got, []any{"keep", nil}) {
		t.Fatalf("list=%v", got)
	}
}

func TestSpanNilAndNonfiniteValues(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	_, s := StartSpan(context.Background(), SpanOptions{Attributes: map[string]any{"nil": nil, "numbers": []float64{math.NaN(), math.Inf(1), math.Inf(-1)}}})
	s.End(nil)
	got := spanAttributes(r.named("ai_otel_span")[0].Properties)
	if v, ok := got["nil"]; !ok || v != nil {
		t.Errorf("nil missing: %v", got)
	}
	if !reflect.DeepEqual(got["numbers"], []any{"NaN", "Infinity", "-Infinity"}) {
		t.Errorf("numbers=%v", got["numbers"])
	}
}
