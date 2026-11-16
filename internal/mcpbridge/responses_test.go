package mcpbridge

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type mcpgoID struct{ v any }

func (i mcpgoID) Value() any { return i.v }

func TestResponsesMatchesIDsByType(t *testing.T) {
	r := NewResponses()
	got := map[string]map[string]any{}
	for name, id := range map[string]any{"number": int64(1), "string": "1"} {
		if !r.Await(id, func(m map[string]any) { got[name] = m }) {
			t.Fatalf("await %s", name)
		}
	}
	if r.Await(mcpgoID{json.Number("1")}, func(map[string]any) {}) {
		t.Fatal("duplicate id awaited twice")
	}
	// A server-to-client request reusing the id does not consume the pending response.
	r.DeliverJSON([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.DeliverJSON([]byte(`{"jsonrpc":"2.0","id":"unrelated","result":{}}`))
	if len(got) != 0 {
		t.Fatalf("delivered early: %v", got)
	}
	r.DeliverJSON([]byte(`[{"jsonrpc":"2.0","id":"1","error":{"code":0,"message":"string"}},{"jsonrpc":"2.0","id":1.0,"error":{"code":0,"message":"number"}}]`))
	if got["string"]["error"].(map[string]any)["message"] != "string" || got["number"]["error"].(map[string]any)["message"] != "number" {
		t.Fatalf("wrong match: %v", got)
	}
	if r.Waiting() {
		t.Fatal("still waiting")
	}
}

func TestResponsesWithoutResponse(t *testing.T) {
	old := ResponseTimeout
	ResponseTimeout = 20 * time.Millisecond
	t.Cleanup(func() { ResponseTimeout = old })
	var mu sync.Mutex
	calls := map[string]int{}
	nilOnly := true
	record := func(name string) func(map[string]any) {
		return func(m map[string]any) {
			mu.Lock()
			defer mu.Unlock()
			calls[name]++
			nilOnly = nilOnly && m == nil
		}
	}
	r := NewResponses()
	r.Await("failed", record("failed"))
	r.Await("timeout", record("timeout"))
	r.Fail("failed")
	time.Sleep(100 * time.Millisecond)
	r.Await("closed", record("closed"))
	r.Close()
	if r.Await("late", record("late")) {
		t.Fatal("await after close")
	}
	// A late response after the timeout runs nothing twice.
	r.DeliverJSON([]byte(`{"jsonrpc":"2.0","id":"timeout","result":{}}`))
	mu.Lock()
	defer mu.Unlock()
	if calls["failed"] != 1 || calls["timeout"] != 1 || calls["closed"] != 1 || calls["late"] != 0 || !nilOnly {
		t.Fatalf("calls=%v nilOnly=%v", calls, nilOnly)
	}
}

func TestResponsesNilAndContext(t *testing.T) {
	var r *Responses
	if r.Await(1, func(map[string]any) {}) {
		t.Fatal("nil observer awaited")
	}
	if NewResponses().Await(nil, func(map[string]any) {}) || NewResponses().Await(true, func(map[string]any) {}) {
		t.Fatal("invalid id awaited")
	}
	obs := NewResponses()
	if ResponsesFrom(WithResponses(context.Background(), obs)) != obs || ResponsesFrom(context.Background()) != nil {
		t.Fatal("context round trip")
	}
}
