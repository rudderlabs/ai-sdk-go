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
// response, or with nil when none can follow (failed write, close, timeout).
type Responses struct {
	mu      sync.Mutex
	waiting map[string]*waiter
	// stale counts responses still to be written for requests whose id a client reused.
	stale  map[string]int
	closed bool
}

type waiter struct {
	done  func(response map[string]any)
	timer *time.Timer
}

// NewResponses returns an open observer.
func NewResponses() *Responses {
	return &Responses{waiting: map[string]*waiter{}, stale: map[string]int{}}
}

// Await registers done for the response to request id. It returns false, and never calls done,
// when r is nil or closed, the id is not a JSON-RPC id, or the id is already awaited.
func (r *Responses) Await(id any, done func(response map[string]any)) bool {
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
			done(nil)
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
func (r *Responses) Deliver(msg map[string]any) {
	if _, ok := msg["method"]; ok {
		return
	}
	_, result := msg["result"]
	_, failed := msg["error"]
	if !result && !failed {
		return
	}
	key, ok := IDKey(msg["id"])
	if !ok {
		return
	}
	r.mu.Lock()
	if r.stale[key] > 0 {
		// The earlier request's response, written after its id was reused.
		if r.stale[key]--; r.stale[key] == 0 {
			delete(r.stale, key)
		}
		r.mu.Unlock()
		return
	}
	w := r.waiting[key]
	delete(r.waiting, key)
	r.mu.Unlock()
	if w != nil {
		w.timer.Stop()
		w.done(msg)
	}
}

// Incoming reads a message the client sent. A request reusing an id whose response is still
// awaited (the server frees an id before writing its response) drops that waiter, and the
// response written for it is then skipped rather than matched to the new request.
func (r *Responses) Incoming(data []byte) {
	for _, msg := range decode(data) {
		if _, ok := msg["method"]; !ok {
			continue
		}
		key, ok := IDKey(msg["id"])
		if !ok {
			continue
		}
		r.mu.Lock()
		w := r.waiting[key]
		if w != nil {
			delete(r.waiting, key)
			r.stale[key]++
		}
		r.mu.Unlock()
		if w != nil {
			w.timer.Stop()
			w.done(nil)
		}
	}
}

// decode reads one JSON-RPC message or a batch.
func decode(data []byte) (out []map[string]any) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		out = append(out, x)
	case []any:
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// DeliverJSON decodes one message or a batch and delivers each response in it.
func (r *Responses) DeliverJSON(data []byte) {
	for _, msg := range decode(data) {
		r.Deliver(msg)
	}
}

// Fail runs the waiter for id without a response: its write failed.
func (r *Responses) Fail(id any) {
	if w := r.pop(id); w != nil {
		w.done(nil)
	}
}

// Close ends observation: every waiter runs without a response, and later Awaits fail.
func (r *Responses) Close() {
	r.mu.Lock()
	r.closed = true
	waiting := r.waiting
	r.waiting = map[string]*waiter{}
	r.stale = map[string]int{}
	r.mu.Unlock()
	for _, w := range waiting {
		w.timer.Stop()
		w.done(nil)
	}
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
