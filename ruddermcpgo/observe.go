package ruddermcpgo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// ObserveHTTP wraps the handler serving a server wrapped with [WrapServer] (typically
// server.NewStreamableHTTPServer), so that failed requests carry error_message (with
// CaptureContent on) taken from the final JSON-RPC response written to the client, as JSON or as
// an SSE frame, matched by request id:
//
//	http.Handle("/mcp", ruddermcpgo.ObserveHTTP(server.NewStreamableHTTPServer(s)))
//
// Without it, error_message is left out: OnAfterCallTool hooks can still change a result after
// WrapServer reads it. Responses pass through unchanged. A failed request's events are sent once
// its response is written, or without error_message when the HTTP request ends first.
func ObserveHTTP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responses := mcpbridge.NewResponses()
		responses.CloseWhenDone(r.Context())
		ow := &observedWriter{ResponseWriter: w, responses: responses}
		defer func() {
			safely(ow.finish)
			responses.Close()
		}()
		var out http.ResponseWriter = ow
		if _, ok := w.(http.Flusher); ok {
			out = flushingWriter{ow}
		}
		h.ServeHTTP(out, r.WithContext(mcpbridge.WithResponses(r.Context(), responses)))
	})
}

// ObserveStdio returns the context, reader and writer to pass to StdioServer.Listen, so that
// failed requests carry error_message as with [ObserveHTTP], read from the newline-delimited
// responses. The reader notices a client reusing a request id before its response is written.
//
//	ctx, in, out := ruddermcpgo.ObserveStdio(ctx, os.Stdin, os.Stdout)
//	err := server.NewStdioServer(s).Listen(ctx, in, out)
//
// Held events are sent without error_message when ctx ends.
func ObserveStdio(ctx context.Context, r io.Reader, w io.Writer) (context.Context, io.Reader, io.Writer) {
	responses := mcpbridge.NewResponses()
	responses.CloseWhenDone(ctx)
	rc, ok := r.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(r)
	}
	return mcpbridge.WithResponses(ctx, responses), mcpbridge.NewReader(rc, responses), mcpbridge.NewWriter(w, responses)
}

type observedWriter struct {
	http.ResponseWriter
	responses *mcpbridge.Responses
	mu        sync.Mutex
	sse       bool
	decided   bool
	keep      bool
	body      []byte
	envelope  mcpbridge.Envelope
	line      mcpbridge.Lines
	data      []byte
	// events are complete SSE events, delivered once a flush commits them.
	events [][]byte
	failed bool
}

func (o *observedWriter) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observedWriter) Write(p []byte) (int, error) {
	safely(o.snapshot)
	n, err := o.ResponseWriter.Write(p)
	if err != nil {
		safely(func() { o.fail(p) })
		return n, err
	}
	safely(func() { o.observe(p[:n]) })
	return n, err
}

func (o *observedWriter) snapshot() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.decided {
		sse := strings.HasPrefix(o.Header().Get("Content-Type"), "text/event-stream")
		o.sse, o.keep, o.decided = sse, o.responses.Waiting(), true
	}
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
	o.envelope.Feed(p)
	if o.keep {
		if len(o.body)+len(p) > mcpbridge.MaxObserved {
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

// fail reports the responses in the failed write, and those written but not yet flushed, as
// failed, then releases every other waiter of this HTTP request without a response.
func (o *observedWriter) fail(attempted []byte) {
	o.mu.Lock()
	var failed [][]byte
	if !o.failed && o.decided {
		switch {
		case o.sse:
			o.line.Feed(attempted, o.responses, o.sseLine)
			failed = o.events
		case o.keep:
			failed = [][]byte{append(o.body, attempted...)}
		default:
			o.envelope.Feed(attempted)
			failed = [][]byte{o.envelope.JSON()}
		}
	}
	o.failed, o.body, o.events = true, nil, nil
	o.mu.Unlock()
	for _, response := range failed {
		o.responses.FailJSON(response)
	}
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
	if !o.keep && !sse {
		body = o.envelope.JSON()
	}
	o.mu.Unlock()
	if failed {
		return
	}
	if sse && pending {
		if err := http.NewResponseController(o.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			o.fail(nil)
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
		safely(func() { f.fail(nil) })
		return err
	}
	safely(f.commit)
	return nil
}
