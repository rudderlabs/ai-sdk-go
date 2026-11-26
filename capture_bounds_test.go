package rudderstackai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
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
	SizeCases []struct {
		Name            string `json:"name"`
		Input           any    `json:"input"`
		ResultSizeBytes int    `json:"result_size_bytes"`
	} `json:"result_size_cases"`
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

// toolResultSize records input as a plain tool result and returns the emitted result_size_bytes.
func toolResultSize(t *testing.T, capture bool, result any) any {
	t.Helper()
	rule := EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", When: func(map[string]any, any) bool { return true }}}}
	r := setupAI(t, WithCaptureContent(capture), WithEventRules(rule))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: result, Error: Ptr(false)})
	i.End(EndOptions{})
	return r.props(t, "looked_up")["result_size_bytes"]
}

func TestResultSizeFixture(t *testing.T) {
	for _, c := range budgetFixture.SizeCases {
		for _, capture := range []bool{false, true} {
			t.Run(c.Name, func(t *testing.T) {
				if got := toolResultSize(t, capture, buildBudget(c.Input)); got != c.ResultSizeBytes {
					t.Fatalf("result_size_bytes=%v, want %d", got, c.ResultSizeBytes)
				}
			})
		}
	}
}

func TestResultSizeSharedMapIsExact(t *testing.T) {
	shared := map[string]any{"html": "<a&b>", "n": []any{1, 2.5}}
	result := map[string]any{"a": shared, "b": shared, "c": []any{shared, shared}}
	b, _ := compactJSON(result)
	if got := toolResultSize(t, true, result); got != len(b) {
		t.Fatalf("result_size_bytes=%v, want %d", got, len(b))
	}
	// Past the budget the size stays exact: x nested in {a: x, b: x} 30 levels.
	deep := sharedMaps(30)
	want := int64(2)
	for range 30 {
		want = 11 + 2*want
	}
	if n, err := exactSize(deep); err != nil || int64(n) != want {
		t.Fatalf("exactSize=%d, %v; want %d", n, err, want)
	}
}

type sizeTagged struct {
	Name  string          `json:"name"`
	Skip  string          `json:"skip,omitempty"`
	Count int             `json:"count,string"`
	Ratio float32         `json:"ratio"`
	Inner *sizeTagged     `json:"inner,omitempty"`
	Raw   json.RawMessage `json:"raw"`
	Blob  []byte          `json:"blob"`
}

type stringPtrs struct {
	I   *int         `json:",string"`
	U   *uint8       `json:",string"`
	F   *float64     `json:",string"`
	B   *bool        `json:",string"`
	S   *string      `json:",string"`
	N   *json.Number `json:",string"`
	Nil *int         `json:",string"`
}

func TestExactSizeMatchesCompactJSON(t *testing.T) {
	shared := &sizeTagged{Name: "<x>&\u2028", Count: 7, Ratio: 0.1, Raw: json.RawMessage(`{"a": 1}`), Blob: []byte("hi")}
	values := []any{
		nil, "text <>&", 1, -3.5, float32(0.1), true, []any{}, map[string]any{}, []string{"a", "b"},
		map[int]string{2: "b", 10: "a"}, map[string]any{"k": []any{1, "x", nil, map[string]any{"z": false}}},
		[]byte("abc"), json.Number("12.50"), shared,
		[]*sizeTagged{shared, shared, {Name: "n", Inner: shared}}, struct{ A, b int }{1, 2},
		map[string]any{"p": shared, "q": shared}, [2]int{1, 2}, (*int)(nil),
		stringPtrs{I: ptr(7), U: ptr(uint8(8)), F: ptr(1.5), B: ptr(true), S: ptr("a\"b"), N: ptr(json.Number("3"))},
	}
	for n, v := range values {
		want, err := compactJSON(v)
		if err != nil {
			t.Fatalf("%d: %v", n, err)
		}
		if got, err := exactSize(v); err != nil || got != len(want) {
			t.Errorf("%d %T: exactSize=%d err=%v, want %d (%s)", n, v, got, err, len(want), want)
		}
	}
	for n, v := range []any{func() {}, make(chan int), complex(1, 2), map[string]any{"f": func() {}}, math.NaN()} {
		if _, err := compactJSON(v); err == nil {
			t.Fatalf("%d: compactJSON accepted %T", n, v)
		}
		if _, err := exactSize(v); err == nil {
			t.Errorf("%d %T: exactSize accepted it", n, v)
		}
	}
	cyc := map[string]any{}
	cyc["self"] = cyc
	if _, err := exactSize(cyc); err == nil {
		t.Error("cycle accepted")
	}
}

type countingMarshaler struct{ calls *int }

func (c *countingMarshaler) MarshalJSON() ([]byte, error) {
	*c.calls++
	return []byte(`{"n":1}`), nil
}

func TestBudgetCutIsNotReusedByAnotherRoot(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	calls := 0
	shared := map[string]any{"a": &countingMarshaler{&calls}, "b": 7}
	first := make([]any, maxWalkNodes-3)
	for n := range first {
		first[n] = "v"
	}
	first = append(first, shared)
	got := captureFields(map[string]any{"first": first, "second": shared}).(map[string]any)
	want := map[string]any{"a": map[string]any{"n": json.Number("1")}, "b": 7}
	if second := got["second"]; !reflect.DeepEqual(second, want) {
		t.Fatalf("second root=%v", second)
	}
	if calls != 1 {
		t.Fatalf("serializer ran %d times", calls)
	}
	if kept := got["first"].([]any); len(kept) != maxWalkNodes-2 {
		t.Fatalf("first root kept %d", len(kept))
	}
}

func TestBudgetCountsEntriesWithDroppedKeys(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	// Interface keys of an unsupported type are dropped, yet each costs budget.
	m := map[any]any{}
	for n := range maxWalkNodes {
		m[[1]int{n}] = n
	}
	m["keep"] = 1
	got := captureContent(m)
	if got == nil {
		t.Fatal("nil")
	}
	if g, ok := got.(map[string]any); !ok || len(g) != 0 {
		t.Fatalf("kept %v", got)
	}
}

func TestOutputToolCallsShareOneBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	items := make([]any, 90000)
	for n := range items {
		items[n] = "v"
	}
	calls := make([]GenAIToolCall, 50)
	for n := range calls {
		calls[n] = GenAIToolCall{Name: "lookup", Arguments: items}
	}
	var content *callContent
	bounded(t, func() { content = captureCallContent(nil, "", calls) })
	total := 0
	for _, c := range content.outputToolCalls {
		total += nodeCount(c)
	}
	if total > maxWalkNodes {
		t.Fatalf("kept %d values", total)
	}
}

func filler(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = "v"
	}
	return out
}

func TestMediaMapCutByBudgetLosesItsPayload(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	// Sorted, "padding" uses the budget before "type" is reached, so the marker is left out.
	media := map[string]any{"base64": "SECRET", "padding": filler(maxWalkNodes), "type": "image"}
	for name, got := range map[string]any{"captureContent": captureContent(media), "stripBinary": stripBinary(media)} {
		if m, ok := got.(map[string]any); ok {
			if _, leaked := m["base64"]; leaked {
				t.Errorf("%s kept the payload", name)
			}
		}
	}
}

func TestMCPControlFieldsSurviveTheBudget(t *testing.T) {
	for _, rule := range []bool{false, true} {
		opts := Options{}
		if rule {
			opts.EventRules = EventRules{Tools: map[string]ToolEventRule{"big": {Event: "big_called"}}}
		}
		server := mcp.NewServer(&mcp.Implementation{Name: "big", Version: "1"}, nil)
		// "_meta" sorts before "isError", so its entries use the whole budget first.
		meta := mcp.Meta{}
		for i := range maxWalkNodes + 10 {
			meta[strconv.FormatInt(int64(i), 36)] = 1
		}
		server.AddTool(&mcp.Tool{Name: "big", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Meta: meta, IsError: true}, nil
		})
		r := wrap(t, server, opts)
		cs := connectMaybeObserved(t, server, true)
		callTool(t, cs, "big", map[string]any{}, nil)
		// Capture is slow under -race and coverage: wait for the event, not for a fixed time.
		var events []analytics.Track
		for deadline := time.Now().Add(2 * time.Minute); len(events) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			events = r.named("mcp_tool_called")
		}
		if len(events) != 1 {
			t.Fatalf("rule=%t: %d mcp_tool_called events: %v", rule, len(events), r.names())
		}
		p := events[0].Properties
		if p["tool_status"] != "error" || p["is_error_result"] != true {
			t.Errorf("rule=%t: an error result was reported as %v: %v", rule, p["tool_status"], p)
		}
	}
}

type onceMarshaler struct{ calls *int }

func (m *onceMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	return []byte(`{"n":1}`), nil
}

func TestSizingNeverRunsSerializersOutsideTheCapture(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	calls := 0
	m := &onceMarshaler{&calls}
	// The budget stops before the marshalers, so the walk never sees them.
	result := append(filler(maxWalkNodes), m, m)
	report := captureValueFields(map[string]any{"result": result})
	if calls > 1 {
		t.Fatalf("serializer ran %d times", calls)
	}
	// A serializer-free sibling stays exact whatever the other field holds.
	plain := filler(maxWalkNodes + 5)
	args := captureValueFields(map[string]any{"args": &onceMarshaler{&calls}, "result": plain})
	want, _ := compactJSON(plain)
	if args.sizes["result"] != len(want) {
		t.Fatalf("plain result size=%d want %d", args.sizes["result"], len(want))
	}
	if report.sizes["result"] <= 0 {
		t.Fatalf("size=%d", report.sizes["result"])
	}
}

func TestPlainResultDeeperThanTheDepthCapIsSizedExactly(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	var v any = "leaf"
	for range 80 {
		v = map[string]any{"c": v}
	}
	want, _ := compactJSON(v)
	for name, got := range map[string]int{"root": captureValue(v).size, "field": captureValueFields(map[string]any{"result": v}).sizes["result"]} {
		if got != len(want) {
			t.Errorf("%s size=%d want %d", name, got, len(want))
		}
	}
}

func TestSliceCaptureAllocatesWithinTheBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	big := make([]int, 5_000_000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := captureContent(big)
	runtime.ReadMemStats(&after)
	if n := len(out.([]any)); n != maxWalkNodes-1 {
		t.Fatalf("kept %d", n)
	}
	if mib := (after.TotalAlloc - before.TotalAlloc) >> 20; mib > 40 {
		t.Fatalf("allocated %d MiB", mib)
	}
}

type fieldHolder struct {
	A, B, C int
	D       *onceMarshaler
}

func TestStructFieldsCountAgainstTheBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	calls := 0
	// A, B and C use the last of the budget; D must be left out unread, its serializer unrun.
	list := append(filler(maxWalkNodes-5), fieldHolder{1, 2, 3, &onceMarshaler{&calls}})
	got := captureContent(list).([]any)
	last, ok := got[len(got)-1].(map[string]any)
	if !ok || len(last) != 3 || calls != 0 {
		t.Fatalf("struct kept %v, serializer ran %d times", got[len(got)-1], calls)
	}
}

func TestSpanAttributesAreSeparateRoots(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	got := captureAttributes(map[string]any{"a": filler(70000), "b": filler(70000)})
	if len(got["a"].([]any)) != 70000 || len(got["b"].([]any)) != 70000 {
		t.Fatal("an attribute's budget was shared")
	}
}

func TestCustomPropertiesAreOneRoot(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	shared := filler(90000)
	props := map[string]any{}
	for i := range 100 {
		props[fmt.Sprintf("p%03d", i)] = shared
	}
	got := captureProperties(props)
	total := 0
	for _, v := range got {
		total += nodeCount(v)
	}
	if total > maxWalkNodes {
		t.Fatalf("kept %d values", total)
	}
}

func TestSizesOfManyKeysSharingOneValueAreCheap(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	shared := filler(90000)
	timed := func(keys int) time.Duration {
		m := map[string]any{}
		for i := range keys {
			m[fmt.Sprintf("k%d", i)] = shared
		}
		started := time.Now()
		if r := captureValue(m); r.size <= 0 {
			t.Fatalf("size=%d", r.size)
		}
		return time.Since(started)
	}
	// The cost is the budget's, not the keys': 2000 keys take about what 2 do, under any instrumentation.
	timed(2)
	one, many := timed(2), timed(2000)
	if many > 5*one+100*time.Millisecond {
		t.Fatalf("2 keys took %v, 2000 keys took %v", one, many)
	}
}

func TestSharedGraphSizesAreExactThroughCapture(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	deep := sharedMaps(30)
	want := 2
	for range 30 {
		want = 11 + 2*want
	}
	bounded(t, func() {
		if got := captureValue(deep).size; got != want {
			t.Errorf("root size=%d want %d", got, want)
		}
		if got := captureValueFields(map[string]any{"k": deep}).sizes["k"]; got != want {
			t.Errorf("key size=%d want %d", got, want)
		}
	})
}

func TestBoundedJSONGivesEachFieldItsOwnBudget(t *testing.T) {
	two := map[string]any{"a": filler(70000), "b": filler(70000)}
	want, _ := compactJSON(two)
	if got, _ := boundedJSON(two, true); len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	if got, _ := boundedJSON(two, false); len(got) >= len(want) {
		t.Fatalf("one budget kept everything: %d", len(got))
	}
}

func TestMemoCopiesAreSharedAcrossEntries(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	shared := []any{1, 2, 3}
	a, b := map[string]any{"x": shared}, map[string]any{"x": shared}
	report := captureValue(map[string]any{"a": a, "b": b})
	ka, _ := serializerIdentity(reflect.ValueOf(a))
	kb, _ := serializerIdentity(reflect.ValueOf(b))
	xa, xb := report.memo[ka].(map[string]any)["x"], report.memo[kb].(map[string]any)["x"]
	if reflect.ValueOf(xa).Pointer() != reflect.ValueOf(xb).Pointer() {
		t.Fatal("each memo entry copied the shared value again")
	}
}

func TestProjectedToolInputWithinTheBudgetIsKeptWhole(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true), WithCaptureContent(true))
	content := captureCallContent(nil, "", []GenAIToolCall{{Name: "lookup", Arguments: map[string]any{"rows": filler(60000)}}})
	args := content.outputToolCalls[0].(map[string]any)["arguments"].(map[string]any)
	if n := len(args["rows"].([]any)); n != 60000 {
		t.Fatalf("kept %d", n)
	}
}

func ptr[T any](v T) *T { return &v }

type taggedMedia struct {
	Base64  string   `json:"base64"`
	Padding []string `json:"padding"`
	Type    string   `json:"type"`
}

func TestStructCutBeforeItsMediaMarkerLosesItsPayload(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	pad := make([]string, maxWalkNodes)
	for name, got := range map[string]any{"captureContent": captureContent(taggedMedia{"SECRET", pad, "image"}), "stripBinary": stripBinary(taggedMedia{"SECRET", pad, "image"})} {
		if m, ok := got.(map[string]any); ok {
			if _, leaked := m["base64"]; leaked {
				t.Errorf("%s kept the payload", name)
			}
		}
	}
}

func TestCustomPropertiesKeepExactlyTheBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	got := captureProperties(map[string]any{"v": filler(maxWalkNodes - 2)})
	if n := len(got["v"].([]any)); n != maxWalkNodes-2 {
		t.Fatalf("kept %d of %d", n, maxWalkNodes-2)
	}
}

func TestShareCopyCutByDepthIsNotReusedHigherUp(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	var shared any = "leaf"
	for range 28 {
		shared = map[string]any{"c": shared}
	}
	deep := shared
	for range 5 {
		deep = map[string]any{"w": deep}
	}
	v := []any{deep, shared} // shared is cut by depth in the first place and fits whole in the second
	plain := (&walk{path: map[visit]bool{}, legacy: false, preserve: true}).stripValue(v)
	copied := (&walk{path: map[visit]bool{}, legacy: false, preserve: true, share: true, copies: map[copyKey]any{}}).stripValue(v)
	if !reflect.DeepEqual(plain, copied) {
		t.Fatalf("the shared pass differs from the plain walk")
	}
}

func (w *walk) stripValue(v any) any {
	out, _ := w.strip(v)
	return out
}

// manyOnes is an app serializer whose output is n ones; it counts its runs.
type manyOnes struct {
	n     int
	calls *int
}

func (m *manyOnes) MarshalJSON() ([]byte, error) {
	if m.calls != nil {
		*m.calls++
	}
	b := make([]byte, 0, 2*m.n+2)
	b = append(b, '[')
	for i := range m.n {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '1')
	}
	return append(b, ']'), nil
}

func TestSerializerResultIsSizedFromItsBudgetedCopy(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	report := captureValueFields(map[string]any{"result": &manyOnes{n: 150000}})
	copied, _ := compactJSON(report.value.(map[string]any)["result"])
	if report.sizes["result"] != len(copied) || len(copied) >= 300001 {
		t.Fatalf("size=%d, the copy is %d bytes", report.sizes["result"], len(copied))
	}
}

func TestManyReferencesToOneSerializerAreCheap(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	m := &manyOnes{n: 90000}
	timed := func(n int) time.Duration {
		refs := make([]any, n)
		for i := range refs {
			refs[i] = m
		}
		started := time.Now()
		report := captureValueFields(map[string]any{"result": refs})
		if report.sizes["result"] <= 0 || report.sizes["result"] > 400000 {
			t.Fatalf("size=%d", report.sizes["result"])
		}
		return time.Since(started)
	}
	// The cost is the budget's, not the references': 100,000 take about what 2 do, under any instrumentation.
	timed(2)
	one, many := timed(2), timed(100000)
	if many > 5*one+100*time.Millisecond {
		t.Fatalf("2 references took %v, 100000 took %v", one, many)
	}
}

func TestCustomPropertiesKeepEveryKeyWithinTheBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	props := map[string]any{}
	for i := range maxWalkNodes - 1 {
		props[strconv.Itoa(i)] = i
	}
	if n := len(captureProperties(props)); n != maxWalkNodes-1 {
		t.Fatalf("kept %d of %d", n, maxWalkNodes-1)
	}
}

func TestShareCopiesDeepGraphUnderManyWrapperDepthsOnce(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	graph := sharedMaps(40) // 2^40 paths, more levels than the depth cap allows
	sw := func() *walk {
		return &walk{path: map[visit]bool{}, legacy: false, preserve: true, share: true, copies: map[copyKey]any{}}
	}
	one := func() time.Duration {
		started := time.Now()
		sw().stripValue(graph)
		return time.Since(started)
	}
	one()
	single := one()
	// One memo copies the graph under several wrapper depths: each (container, depth) once.
	w := sw()
	started := time.Now()
	for k := range 8 {
		v := graph
		for range k {
			v = map[string]any{"w": v}
		}
		w.stripValue(v)
	}
	if all := time.Since(started); all > 100*single+200*time.Millisecond {
		t.Fatalf("one depth took %v, eight took %v", single, all)
	}
}

// serResult is a tool result whose own MarshalJSON writes the wire bytes; its fields are not them.
type serResult struct {
	calls   *int
	raw     string
	IsError bool `json:"isError,omitempty"`
}

func (s *serResult) MarshalJSON() ([]byte, error) {
	*s.calls++
	return []byte(s.raw), nil
}

func ones(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("1,", n), ",") + "]" }

// toolOutcome runs one MCP tools/call whose result is result and returns the emitted
// mcp_tool_called properties and the call's context.
func toolOutcome(t *testing.T, result any) (map[string]any, *reqCtx) {
	t.Helper()
	s := workspaceServer("controls", nil)
	r := wrap(t, s, Options{})
	w, _ := instances.get(s)
	tools := []mcpbridge.Tool{{Name: "t", Schema: map[string]any{"type": "object"}}}
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools})
	rc.ToolCall("t", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{Result: result})
	events := r.named("mcp_tool_called")
	if len(events) == 0 {
		return nil, rc
	}
	return events[len(events)-1].Properties, rc
}

func TestSerializerResultKeepsItsErrorWhenTheCopyIsCut(t *testing.T) {
	calls := 0
	p, _ := toolOutcome(t, &serResult{calls: &calls, raw: `{"_meta":{"pad":` + ones(150000) + `},"isError":true}`})
	if p["tool_status"] != "error" || p["is_error_result"] != true || calls != 1 {
		t.Fatalf("status=%v is_error=%v, serializer ran %d times", p["tool_status"], p["is_error_result"], calls)
	}
}

func TestSerializerOmittedErrorStaysOmittedWhenTheCopyIsCut(t *testing.T) {
	calls := 0
	// The receiver says isError, the serializer's bytes do not: the bytes are the result.
	p, _ := toolOutcome(t, &serResult{calls: &calls, raw: `{"content":` + ones(150000) + `}`, IsError: true})
	if p["tool_status"] != "success" || p["is_error_result"] != false {
		t.Fatalf("status=%v is_error=%v", p["tool_status"], p["is_error_result"])
	}
}

func TestInputRequestCutToAnEmptyMapStillRegistersItsKey(t *testing.T) {
	calls := 0
	// The copy's budget ends before the entry's method, leaving it an empty map.
	raw := `{"content":` + ones(maxWalkNodes-4) + `,"inputRequests":{"k":{"method":"elicitation/create"}},"requestState":"s"}`
	_, rc := toolOutcome(t, &serResult{calls: &calls, raw: raw})
	if _, found := rc.st.inputRequired.get(rc.inputKey("k", "s")); !found {
		t.Fatal("the pending key was not registered")
	}
}

func TestCustomPropertiesCountEntriesTheCaptureLeavesOut(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	props := map[string]any{"z": 1}
	for i := range maxWalkNodes - 1 {
		props[fmt.Sprintf("a%06d", i)] = func() {}
	}
	if _, kept := captureProperties(props)["z"]; kept {
		t.Fatal("a scalar survived after functions spent the whole budget")
	}
}

type bufferShaped struct {
	Data []int  `json:"data"`
	Type string `json:"type"`
}

func TestCutBufferLosesItsBytes(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	pad := make([]int, maxWalkNodes)
	for name, v := range map[string]any{
		"map":    map[string]any{"data": filler(maxWalkNodes), "type": "Buffer"},
		"struct": bufferShaped{pad, "Buffer"},
	} {
		got := captureContent(map[string]any{"content": v}).(map[string]any)
		if m, ok := got["content"].(map[string]any); ok {
			if _, leaked := m["data"]; leaked {
				t.Errorf("%s kept the bytes", name)
			}
		}
	}
}

type marshalFunc func()

var marshalFuncCalls int

func (marshalFunc) MarshalJSON() ([]byte, error) { marshalFuncCalls++; return []byte(`"f"`), nil }

type marshalChan chan int

func (marshalChan) MarshalText() ([]byte, error) { marshalFuncCalls++; return []byte("c"), nil }

func TestNonDataValuesAreLeftOutBeforeTheirSerializer(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	marshalFuncCalls = 0
	got := captureContent(map[string]any{"f": marshalFunc(func() {}), "c": make(marshalChan), "keep": 1}).(map[string]any)
	if len(got) != 1 || marshalFuncCalls != 0 {
		t.Fatalf("captured %v, serializers ran %d times", got, marshalFuncCalls)
	}
}

func TestResultStatsComeFromTheWholeResultWhenTheCopyIsCut(t *testing.T) {
	whole := func() map[string]any {
		return map[string]any{
			"_meta":             map[string]any{"pad": filler(maxWalkNodes + 5)},
			"content":           []any{map[string]any{"type": "text", "text": "x"}},
			"structuredContent": map[string]any{"nextCursor": "c"},
		}
	}
	raw, _ := json.Marshal(whole())
	calls := 0
	for name, result := range map[string]any{"plain": whole(), "serializer": &serResult{calls: &calls, raw: string(raw)}} {
		p, _ := toolOutcome(t, result)
		if !reflect.DeepEqual(p["result_content_types"], []string{"text", "structured"}) || p["result_paginated"] != true {
			t.Errorf("%s: types=%v paginated=%v", name, p["result_content_types"], p["result_paginated"])
		}
	}
}

func TestRuleInputsComeFromTheWholeResultWhenTheCopyIsCut(t *testing.T) {
	whole := func() map[string]any {
		return map[string]any{"_meta": map[string]any{"pad": filler(maxWalkNodes + 5)}, "structuredContent": map[string]any{"ok": true}}
	}
	raw, _ := json.Marshal(whole())
	calls := 0
	for name, result := range map[string]any{"plain": whole(), "serializer": &serResult{calls: &calls, raw: string(raw)}} {
		var seen any
		rule := EventRules{Tools: map[string]ToolEventRule{"t": {Event: "t_called", When: func(_ map[string]any, result any) bool { seen = result; return true }}}}
		s := workspaceServer("rules", nil)
		wrap(t, s, Options{EventRules: rule})
		w, _ := instances.get(s)
		tools := []mcpbridge.Tool{{Name: "t", Schema: map[string]any{"type": "object"}}}
		rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RegisteredTools: tools})
		rc.ToolCall("t", nil, false, nil, "")
		rc.ToolDone(mcpbridge.Outcome{Result: result})
		if !reflect.DeepEqual(seen, map[string]any{"ok": true}) {
			t.Errorf("%s: the rule saw %v", name, seen)
		}
	}
}

func TestSessionIDSurvivesAnAppContextPastTheBudget(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	got := captureMessageContext(map[string]any{"a": filler(maxWalkNodes + 5), "sessionId": int64(7)})
	if got["sessionId"] != int64(7) {
		t.Fatalf("context=%v", got["sessionId"])
	}
}

func TestSpanSummaryInputsSurviveALargeAttribute(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	// The summary's usage, model and IDs come from the captured attributes, each under its own budget.
	got := captureAttributes(map[string]any{
		"aaa.big": filler(maxWalkNodes + 5), "gen_ai.usage.input_tokens": 7, "gen_ai.request.model": "m",
		"gen_ai.conversation.id": "c1", "rudderstack.interaction_id": "i1", "zzz.finish": []string{"stop"},
	})
	if got["gen_ai.usage.input_tokens"] != 7 || got["gen_ai.request.model"] != "m" || got["gen_ai.conversation.id"] != "c1" ||
		got["rudderstack.interaction_id"] != "i1" || !reflect.DeepEqual(got["zzz.finish"], []any{"stop"}) {
		t.Fatalf("attributes=%v", got)
	}
}
