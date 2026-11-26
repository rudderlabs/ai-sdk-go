package rudderstackai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func nodeCount(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 1
		for _, e := range x {
			n += nodeCount(e)
		}
		return n
	case []any:
		n := 1
		for _, e := range x {
			n += nodeCount(e)
		}
		return n
	}
	return 1
}

// bounded runs f and fails when it takes long or allocates a lot; the caps are loose, only a runaway trips them.
func bounded(t *testing.T, f func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	f()
	runtime.ReadMemStats(&after)
	if d := time.Since(started); d > 20*time.Second {
		t.Fatalf("took %v", d)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<30 {
		t.Fatalf("allocated %d MiB", alloc>>20)
	}
}

func sharedMaps(depth int) any {
	var v any = map[string]any{}
	for range depth {
		v = map[string]any{"a": v, "b": v}
	}
	return v
}

func sharedArrays(depth int) any {
	var v any = []any{}
	for range depth {
		v = []any{v, v}
	}
	return v
}

func TestCaptureBudgetSharedGraphs(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	graphs := map[string]any{"objects": sharedMaps(40), "arrays": sharedArrays(40)}
	for name, graph := range graphs {
		t.Run(name, func(t *testing.T) {
			captures := map[string]func() any{
				"captureContent": func() any { return captureContent(graph) },
				"captureValue":   func() any { return captureValue(graph).value },
				"stripBinary":    func() any { return stripBinary(graph) },
				"spanContext": func() any {
					ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"v": graph}})
					_, s := beginWrapperSpan(ctx, i, time.Now())
					return s.msgContext
				},
			}
			for label, capture := range captures {
				t.Run(label, func(t *testing.T) {
					var out any
					bounded(t, func() { out = capture() })
					if n := nodeCount(out); n > maxWalkNodes {
						t.Fatalf("kept %d values", n)
					}
				})
			}
		})
	}
}

var budgetFixture = func() (f struct {
	MaxNodes int `json:"max_nodes"`
	Cases    []struct {
		Name           string `json:"name"`
		Input          any    `json:"input"`
		Expected       any    `json:"expected"`
		ExpectedNodes  int    `json:"expected_nodes"`
		ExpectedSHA256 string `json:"expected_sha256"`
	} `json:"cases"`
}) {
	b, err := os.ReadFile("spec/fixtures/capture-budget-v0.1.json")
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		panic(err)
	}
	return f
}()

func jsonInt(v any) int { return int(v.(float64)) }

// buildBudget makes the fixture's descriptors: $repeat and $nest refer to one built value from many places.
func buildBudget(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for n, e := range x {
			out[n] = buildBudget(e)
		}
		return out
	case map[string]any:
		if r, ok := x["$repeat"]; ok {
			one := buildBudget(r)
			out := make([]any, jsonInt(x["times"]))
			for n := range out {
				out[n] = one
			}
			return out
		}
		if r, ok := x["$nest"]; ok {
			cur := buildBudget(r)
			for range jsonInt(x["depth"]) {
				if keys, ok := x["keys"].([]any); ok {
					m := map[string]any{}
					for _, k := range keys {
						m[k.(string)] = cur
					}
					cur = m
				} else {
					list := make([]any, jsonInt(x["width"]))
					for n := range list {
						list[n] = cur
					}
					cur = list
				}
			}
			return cur
		}
		out := map[string]any{}
		for k, e := range x {
			out[k] = buildBudget(e)
		}
		return out
	}
	return v
}

func TestCaptureBudgetFixture(t *testing.T) {
	if budgetFixture.MaxNodes != maxWalkNodes {
		t.Fatalf("fixture max_nodes=%d, constant=%d", budgetFixture.MaxNodes, maxWalkNodes)
	}
	for _, emitSpans := range []bool{false, true} {
		setupAI(t, WithEmitSpans(emitSpans))
		for _, c := range budgetFixture.Cases {
			t.Run(c.Name, func(t *testing.T) {
				// "v" in the descriptors is a plain string leaf.
				var got any
				bounded(t, func() { got = captureContent(buildBudget(c.Input)) })
				if c.Expected != nil {
					if want := buildBudget(c.Expected); !reflect.DeepEqual(got, want) {
						t.Fatalf("captured %d values, want %d", nodeCount(got), nodeCount(want))
					}
					return
				}
				if n := nodeCount(got); n != c.ExpectedNodes {
					t.Fatalf("nodes=%d, want %d", n, c.ExpectedNodes)
				}
				b, err := json.Marshal(got) // encoding/json sorts object keys
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(b)
				if hex.EncodeToString(sum[:]) != c.ExpectedSHA256 {
					t.Fatalf("sha256=%x", sum)
				}
			})
		}
	}
}

func TestCaptureBudgetPerField(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	big := buildBudget(map[string]any{"$repeat": "v", "times": float64(70000)})
	got := captureFields(map[string]any{"a": big, "b": big}).(map[string]any)
	for _, k := range []string{"a", "b"} {
		if n := len(got[k].([]any)); n != 70000 {
			t.Fatalf("%s kept %d", k, n)
		}
	}
	if n := len(captureValue(map[string]any{"a": big, "b": big}).value.(map[string]any)["b"].([]any)); n == 70000 {
		t.Fatal("one root should share its budget")
	}
}

func TestCaptureBudgetSizesAndEncoding(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	for _, graph := range []any{sharedMaps(40), sharedArrays(40)} {
		bounded(t, func() {
			r := captureValueFields(map[string]any{"args": graph, "result": graph})
			if r.size <= 0 || r.sizes["args"] <= 0 {
				t.Fatalf("size=%d sizes=%v", r.size, r.sizes)
			}
		})
		bounded(t, func() {
			if b, _ := captureEncoded(graph); len(b) == 0 {
				t.Fatal("no encoding")
			}
		})
		bounded(t, func() {
			captureResolved(graph, func(resolved any) {
				if b, _ := boundedJSON(resolved, false); len(b) == 0 {
					t.Fatal("no encoding")
				}
			})
		})
	}
}

func TestBudgetChangesOnlyCapturedContent(t *testing.T) {
	for _, spans := range []bool{false, true} {
		r := setupAI(t, WithEmitSpans(spans), WithLLMCallEvents(true), WithCaptureContent(true))
		for _, args := range []any{map[string]any{"q": "small"}, sharedMaps(40)} {
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 1200, OutputTokens: 300, CostUSD: Ptr(0.004),
				OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: args}}})
			i.End(EndOptions{})
		}
		varying := map[string]bool{"conversation_id": true, "interaction_id": true, "latency_ms": true, "output_tool_calls": true,
			"trace_id": true, "span_id": true, "timestamp": true, "created_at": true}
		for _, name := range []string{"ai_llm_response_received", "ai_response_received"} {
			events := r.named(name)
			if len(events) != 2 {
				t.Fatalf("spans=%t %s: %d events", spans, name, len(events))
			}
			trim := func(p map[string]any) map[string]any {
				out := map[string]any{}
				for k, v := range p {
					if !varying[k] {
						out[k] = v
					}
				}
				return out
			}
			small, capped := trim(events[0].Properties), trim(events[1].Properties)
			for _, k := range []string{"input_tokens", "output_tokens", "cost_usd", "total_input_tokens", "total_cost_usd", "llm_call_count", "tool_call_count"} {
				if !reflect.DeepEqual(small[k], capped[k]) {
					t.Errorf("spans=%t %s.%s: small=%v capped=%v", spans, name, k, small[k], capped[k])
				}
			}
			if v, ok := small["input_tokens"]; name == "ai_llm_response_received" && (!ok || v == nil) {
				t.Errorf("no input_tokens in %v", small)
			}
		}
	}
}

// TestBudgetCost logs the wall time and heap one call takes at the budget.
func TestBudgetCost(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true), WithCaptureContent(true))
	measure := func(name string, f func()) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := time.Now()
		f()
		d := time.Since(started)
		runtime.ReadMemStats(&after)
		t.Logf("%s: %v, %d MiB allocated, heap in use %+d MiB", name, d.Round(time.Millisecond), (after.TotalAlloc-before.TotalAlloc)>>20, (int64(after.HeapAlloc)-int64(before.HeapAlloc))>>20)
	}
	graph := sharedMaps(40)
	measure("single-value capture, 40-deep {a:x,b:x}", func() { captureContent(graph) })
	measure("recorded call, tool arguments at the budget", func() {
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: graph}}})
		i.End(EndOptions{})
	})
}
