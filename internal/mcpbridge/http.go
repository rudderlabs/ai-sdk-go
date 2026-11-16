package mcpbridge

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// ServeHTTP serves r with h and delivers to responses the JSON-RPC responses h writes, as a JSON
// body or as SSE frames, then closes responses. Responses pass through unchanged.
func ServeHTTP(h http.Handler, w http.ResponseWriter, r *http.Request, responses *Responses) {
	ow := &observedWriter{ResponseWriter: w, responses: responses}
	defer func() {
		safely(ow.finish)
		responses.Close()
	}()
	var out http.ResponseWriter = ow
	if canFlush(w) {
		out = flushingWriter{ow}
	}
	h.ServeHTTP(out, r)
}

// canFlush reports whether w, or a writer it unwraps to, flushes. http.ResponseController
// flushes through Unwrap, so the observer must offer Flush whenever that would succeed, or the
// flush skips it.
func canFlush(w http.ResponseWriter) bool {
	for w != nil {
		switch x := w.(type) {
		case http.Flusher, interface{ FlushError() error }:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = x.Unwrap()
		default:
			return false
		}
	}
	return false
}

func safely(f func()) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "safely")
		}
	}()
	f()
}

type observedWriter struct {
	http.ResponseWriter
	responses *Responses
	mu        sync.Mutex
	sse       bool
	decided   bool
	keep      bool
	body      []byte
	line      Lines
	data      []byte
	// events are complete SSE events, delivered once a flush commits them.
	events [][]byte
	failed bool
}

func (o *observedWriter) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observedWriter) Write(p []byte) (int, error) {
	n, err := o.ResponseWriter.Write(p)
	if err != nil {
		o.fail()
		return n, err
	}
	safely(func() { o.observe(p[:n]) })
	return n, err
}

func (o *observedWriter) observe(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed {
		return
	}
	if !o.decided {
		o.decided = true
		o.sse = strings.HasPrefix(o.Header().Get("Content-Type"), "text/event-stream")
		o.keep = o.responses.Waiting()
	}
	if o.sse {
		o.line.Feed(p, o.responses, o.sseLine)
		return
	}
	if o.keep {
		if len(o.body)+len(p) > MaxObserved {
			o.keep, o.body = false, nil
			return
		}
		o.body = append(o.body, p...)
	}
}

// sseLine collects an event's data lines and delivers the event at its blank line.
func (o *observedWriter) sseLine(line []byte) {
	switch {
	case len(line) == 0:
		if len(o.data) > 0 {
			o.events = append(o.events, append([]byte(nil), o.data...))
		}
		o.data = o.data[:0]
	case bytes.HasPrefix(line, []byte("data:")):
		value := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
		if len(o.data) > 0 {
			o.data = append(o.data, '\n')
		}
		o.data = append(o.data, value...)
	}
}

// fail releases every waiter of this HTTP request without a response: a write or flush
// failed, so the client may not get what was accepted.
func (o *observedWriter) fail() {
	o.mu.Lock()
	o.failed, o.body, o.events = true, nil, nil
	o.mu.Unlock()
	o.responses.Close()
}

// commit delivers the SSE events written so far, once a flush or the handler's end commits them.
func (o *observedWriter) commit() {
	o.mu.Lock()
	events := o.events
	o.events = nil
	o.mu.Unlock()
	for _, event := range events {
		o.responses.DeliverJSON(event)
	}
}

// finish delivers a JSON body, and SSE events not yet flushed, once the handler is done. Some
// mcp-go versions (v0.55.1) write the final SSE event without flushing it, so the stream is
// flushed here first: an event is delivered only once committed.
func (o *observedWriter) finish() {
	o.mu.Lock()
	failed, body, sse, pending := o.failed, o.body, o.sse, len(o.events) > 0
	o.mu.Unlock()
	if failed {
		return
	}
	if sse && pending {
		if err := http.NewResponseController(o.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			o.fail()
			return
		}
	}
	if !sse && len(body) > 0 {
		o.responses.DeliverJSON(body)
	}
	o.commit()
}

type flushingWriter struct{ *observedWriter }

func (f flushingWriter) Flush() { _ = f.FlushError() }

// FlushError reports the underlying writer's flush error (http.ResponseController uses it) and
// delivers the flushed SSE events only when the flush succeeds.
func (f flushingWriter) FlushError() error {
	if err := http.NewResponseController(f.ResponseWriter).Flush(); err != nil {
		f.fail()
		return err
	}
	safely(f.commit)
	return nil
}
