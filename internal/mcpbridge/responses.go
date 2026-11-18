package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

// ResponseTimeout bounds how long a held event waits for its response write, in case a
// transport never writes one. A variable for tests.
var ResponseTimeout = 10 * time.Second

// Responses matches the JSON-RPC responses one connection or HTTP request writes to the client
// with the tracked requests awaiting them, by request id. Each waiter runs exactly once: with the
// response, or with nil when none is seen (close, timeout). A response whose write failed is
// passed with failed set.
type Responses struct {
	mu      sync.Mutex
	waiting map[string]*waiter
	closed  bool
	stop    func() bool // stops the CloseWhenDone callback
}

type waiter struct {
	done  func(response map[string]any, failed bool)
	timer *time.Timer
}

// run calls done, containing a panic: waiters run on the application's writer and timer
// goroutines, where telemetry must never fail the transport.
func (w *waiter) run(response map[string]any, failed bool) {
	defer func() { _ = recover() }()
	w.done(response, failed)
}

// NewResponses returns an open observer.
func NewResponses() *Responses {
	return &Responses{waiting: map[string]*waiter{}}
}

// Await registers done for the response to request id; a response whose write failed is passed
// as nil. It returns false, and never calls done, when r is nil or closed, the id is not a
// JSON-RPC id, or the id is already awaited.
func (r *Responses) Await(id any, done func(response map[string]any)) bool {
	return r.AwaitWrite(id, func(response map[string]any, failed bool) {
		if failed {
			response = nil
		}
		done(response)
	})
}

// AwaitWrite is Await, also reporting whether the response's write failed.
func (r *Responses) AwaitWrite(id any, done func(response map[string]any, failed bool)) bool {
	key, ok := IDKey(id)
	if r == nil || !ok {
		return false
	}
	w := &waiter{done: done}
	r.mu.Lock()
	if r.closed || r.waiting[key] != nil {
		r.mu.Unlock()
		return false
	}
	r.waiting[key] = w
	w.timer = time.AfterFunc(ResponseTimeout, func() {
		if r.take(key, w) {
			w.run(nil, false)
		}
	})
	r.mu.Unlock()
	return true
}

func (r *Responses) take(key string, w *waiter) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiting[key] != w {
		return false
	}
	delete(r.waiting, key)
	return true
}

func (r *Responses) pop(id any) *waiter {
	key, ok := IDKey(id)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.waiting[key]
	delete(r.waiting, key)
	if w != nil {
		w.timer.Stop()
	}
	return w
}

// Waiting reports whether any request awaits its response, so writers parse output only then.
func (r *Responses) Waiting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiting) > 0
}

// Deliver hands a decoded message to the request awaiting its id. Only responses (an id with a
// result or an error, and no method) match: a server-to-client request may reuse an id.
func (r *Responses) Deliver(msg map[string]any) { r.deliver(msg, false) }

func (r *Responses) deliver(msg map[string]any, failed bool) {
	if _, ok := msg["method"]; ok {
		return
	}
	_, result := msg["result"]
	_, rpcError := msg["error"]
	if !result && !rpcError {
		return
	}
	if w := r.pop(msg["id"]); w != nil {
		w.run(msg, failed)
	}
}

// Incoming reads a message the client sent. MCP forbids reusing a request id within a session;
// a request that does so while an earlier response is awaited releases that waiter without a
// response, so the earlier event goes out without error_message.
func (r *Responses) Incoming(data []byte) {
	for _, msg := range decode(data) {
		if _, ok := msg["method"]; ok {
			r.Fail(msg["id"])
		}
	}
}

// decode reads one JSON-RPC message or a batch. A response's _result_size is the written
// length of its result, so sizes follow the bytes the client got.
func decode(data []byte) (out []map[string]any) {
	data = bytes.TrimSpace(data)
	if bytes.HasPrefix(data, []byte("[")) {
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return nil
		}
		for _, item := range items {
			if m := decodeMessage(item); m != nil {
				out = append(out, m)
			}
		}
		return out
	}
	if m := decodeMessage(data); m != nil {
		out = append(out, m)
	}
	return out
}

func decodeMessage(data []byte) map[string]any {
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) != nil {
		return nil
	}
	m := make(map[string]any, len(members))
	for k, raw := range members {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return nil
		}
		m[k] = v
	}
	if result, ok := members["result"]; ok && m["_result_size"] == nil {
		m["_result_size"] = len(result)
	}
	return m
}

// DeliverJSON decodes one message or a batch and delivers each response in it.
func (r *Responses) DeliverJSON(data []byte) {
	for _, msg := range decode(data) {
		r.Deliver(msg)
	}
}

// FailJSON passes each response in a message or batch whose write failed to its waiter.
func (r *Responses) FailJSON(data []byte) {
	for _, msg := range decode(data) {
		r.deliver(msg, true)
	}
}

// Fail runs the waiter for id without a response: none will be matched to it.
func (r *Responses) Fail(id any) {
	if w := r.pop(id); w != nil {
		w.run(nil, false)
	}
}

// Close ends observation: every waiter runs without a response, and later Awaits fail.
func (r *Responses) Close() {
	r.mu.Lock()
	r.closed = true
	waiting := r.waiting
	r.waiting = map[string]*waiter{}
	stop := r.stop
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	for _, w := range waiting {
		w.timer.Stop()
		w.run(nil, false)
	}
}

// CloseWhenDone closes r when ctx ends; Close stops the callback, so ctx keeps nothing alive.
func (r *Responses) CloseWhenDone(ctx context.Context) {
	stop := context.AfterFunc(ctx, r.Close)
	r.mu.Lock()
	r.stop = stop
	r.mu.Unlock()
}

func (r *Responses) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waiting)
}

// IDKey is a comparable form of a JSON-RPC id: strings and numbers stay distinct ("1" is not 1),
// and a number matches however it was decoded. Values with a Value() method (mcp-go's RequestId)
// are unwrapped.
func IDKey(id any) (string, bool) {
	if v, ok := id.(interface{ Value() any }); ok {
		id = v.Value()
	}
	switch v := id.(type) {
	case string:
		return "s:" + v, true
	case int:
		return "n:" + strconv.Itoa(v), true
	case int64:
		return "n:" + strconv.FormatInt(v, 10), true
	case float64:
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64), true
	case json.Number:
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return "n:" + strconv.FormatInt(i, 10), true
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return "n:" + strconv.FormatFloat(f, 'f', -1, 64), true
		}
	}
	return "", false
}

type responsesKey struct{}

// WithResponses carries an observer to the adapter's hooks through the request context.
func WithResponses(ctx context.Context, r *Responses) context.Context {
	return context.WithValue(ctx, responsesKey{}, r)
}

// ResponsesFrom is the observer WithResponses added, or nil.
func ResponsesFrom(ctx context.Context) *Responses {
	r, _ := ctx.Value(responsesKey{}).(*Responses)
	return r
}
