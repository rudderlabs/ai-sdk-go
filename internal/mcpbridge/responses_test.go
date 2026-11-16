package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
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

// A client may reuse an id once the server removed it from its in-flight set: the earlier
// request's waiter is dropped and its pending response is not taken for the new request's.
func TestResponsesReusedID(t *testing.T) {
	r := NewResponses()
	var first, second map[string]any
	firstCalls := 0
	r.Await(int64(1), func(m map[string]any) { firstCalls++; first = m })
	r.Incoming([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
	if firstCalls != 1 || first != nil {
		t.Fatalf("first waiter: calls=%d msg=%v", firstCalls, first)
	}
	if !r.Await(int64(1), func(m map[string]any) { second = m }) {
		t.Fatal("await reused id")
	}
	r.DeliverJSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":0,"message":"first"}}`))
	if second != nil {
		t.Fatalf("second took the first response: %v", second)
	}
	r.DeliverJSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":0,"message":"second"}}`))
	if second["error"].(map[string]any)["message"] != "second" {
		t.Fatalf("second: %v", second)
	}
	// Incoming responses (to server requests) and notifications never drop a waiter.
	called := false
	r.Await("x", func(map[string]any) { called = true })
	r.Incoming([]byte(`[{"jsonrpc":"2.0","id":"x","result":{}},{"jsonrpc":"2.0","method":"notifications/initialized"}]`))
	if called {
		t.Fatal("waiter dropped by a non-request")
	}
}

func TestLineWriterAndReader(t *testing.T) {
	r := NewResponses()
	var got map[string]any
	r.Await("w", func(m map[string]any) { got = m })
	var out bytes.Buffer
	w := NewWriter(nopWriteCloser{&out}, r)
	// Split across writes, with a line written before anything awaited it.
	for _, chunk := range []string{`{"jsonrpc":"2.0","id":"w",`, `"result":{"isError":true}}` + "\n"} {
		if _, err := io.WriteString(w, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if got == nil || out.Len() == 0 {
		t.Fatalf("writer: got=%v out=%q", got, out.String())
	}
	reused := false
	r.Await("r", func(map[string]any) { reused = true })
	rd := NewReader(io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":"r","method":"ping"}`+"\n")), r)
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("reader did not see the reused id")
	}
	closed := false
	r.Await("c", func(map[string]any) { closed = true })
	if err := w.Close(); err != nil || !closed {
		t.Fatalf("close: %v %v", err, closed)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
