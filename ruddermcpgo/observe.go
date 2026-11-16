package ruddermcpgo

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// maxObserved bounds the response bytes kept per message while reading it; a longer message
// leaves error_message out.
const maxObserved = 1 << 20

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

// ObserveStdio returns the context and writer to pass to StdioServer.Listen, so that failed
// requests carry error_message as with [ObserveHTTP], read from the newline-delimited responses:
//
//	ctx, out := ruddermcpgo.ObserveStdio(ctx, os.Stdout)
//	err := server.NewStdioServer(s).Listen(ctx, os.Stdin, out)
//
// Held events are sent without error_message when ctx ends.
func ObserveStdio(ctx context.Context, w io.Writer) (context.Context, io.Writer) {
	responses := mcpbridge.NewResponses()
	context.AfterFunc(ctx, responses.Close)
	return mcpbridge.WithResponses(ctx, responses), &lineWriter{w: w, responses: responses}
}

// lineWriter reads newline-delimited JSON-RPC messages as they are written.
type lineWriter struct {
	mu        sync.Mutex
	w         io.Writer
	responses *mcpbridge.Responses
	line      lines
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.w.Write(p)
	safely(func() { l.line.feed(p[:n], l.responses, l.responses.DeliverJSON) })
	return n, err
}

// lines splits written bytes into lines, keeping a line only when a request awaits its response
// at the line's start.
type lines struct {
	buf      []byte
	started  bool
	keep     bool
	overflow bool
}

func (s *lines) feed(p []byte, responses *mcpbridge.Responses, line func([]byte)) {
	for len(p) > 0 {
		if !s.started {
			s.started, s.keep, s.overflow, s.buf = true, responses.Waiting(), false, s.buf[:0]
		}
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if s.keep && !s.overflow {
			if len(s.buf)+len(chunk) > maxObserved {
				s.overflow, s.buf = true, nil
			} else {
				s.buf = append(s.buf, chunk...)
			}
		}
		if i < 0 {
			return
		}
		if s.keep && !s.overflow {
			line(bytes.TrimSuffix(s.buf, []byte("\r")))
		}
		s.started = false
		p = p[i+1:]
	}
}

type observedWriter struct {
	http.ResponseWriter
	responses *mcpbridge.Responses
	mu        sync.Mutex
	sse       bool
	decided   bool
	keep      bool
	body      []byte
	line      lines
	data      []byte
}

func (o *observedWriter) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observedWriter) Write(p []byte) (int, error) {
	n, err := o.ResponseWriter.Write(p)
	safely(func() { o.observe(p[:n]) })
	return n, err
}

func (o *observedWriter) observe(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.decided {
		o.decided = true
		o.sse = strings.HasPrefix(o.Header().Get("Content-Type"), "text/event-stream")
		o.keep = o.responses.Waiting()
	}
	if o.sse {
		o.line.feed(p, o.responses, o.sseLine)
		return
	}
	if o.keep {
		if len(o.body)+len(p) > maxObserved {
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
			o.responses.DeliverJSON(o.data)
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

// finish delivers a JSON body once the handler is done writing it.
func (o *observedWriter) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.sse && len(o.body) > 0 {
		o.responses.DeliverJSON(o.body)
	}
}

type flushingWriter struct{ *observedWriter }

func (f flushingWriter) Flush() { f.ResponseWriter.(http.Flusher).Flush() }
