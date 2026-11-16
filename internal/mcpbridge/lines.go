package mcpbridge

import (
	"bytes"
	"io"
	"sync"
)

// MaxObserved bounds the bytes kept of one message while reading it; a longer message is
// skipped, and its request's error_message left out.
const MaxObserved = 1 << 20

// Lines splits a stream into lines. A line is kept only when a request awaits its response at
// the line's start, so idle traffic is never buffered.
type Lines struct {
	buf      []byte
	started  bool
	keep     bool
	overflow bool
}

// Feed passes each complete kept line, without its line ending, to line.
func (s *Lines) Feed(p []byte, r *Responses, line func([]byte)) {
	for len(p) > 0 {
		if !s.started {
			s.started, s.keep, s.overflow, s.buf = true, r.Waiting(), false, s.buf[:0]
		}
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if s.keep && !s.overflow {
			if len(s.buf)+len(chunk) > MaxObserved {
				s.overflow, s.buf = true, nil
			} else {
				s.buf = append(s.buf, chunk...)
			}
		}
		if i < 0 {
			return
		}
		if s.keep && !s.overflow {
			guard(func() { line(bytes.TrimSuffix(s.buf, []byte("\r"))) })
		}
		s.started = false
		p = p[i+1:]
	}
}

func guard(f func()) {
	defer func() { _ = recover() }()
	f()
}

// Writer delivers the newline-delimited responses written through it, once written. Closing it
// closes the observer and the underlying writer, if it is a Closer.
type Writer struct {
	mu    sync.Mutex
	w     io.Writer
	r     *Responses
	lines Lines
}

// NewWriter observes the responses written to w.
func NewWriter(w io.Writer, r *Responses) *Writer { return &Writer{w: w, r: r} }

func (x *Writer) Write(p []byte) (int, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	n, err := x.w.Write(p)
	x.lines.Feed(p[:n], x.r, x.r.DeliverJSON)
	return n, err
}

func (x *Writer) Close() error {
	x.r.Close()
	if c, ok := x.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// Reader passes the client's newline-delimited messages to Responses.Incoming.
type Reader struct {
	rc    io.ReadCloser
	r     *Responses
	lines Lines
}

// NewReader observes the messages read from rc.
func NewReader(rc io.ReadCloser, r *Responses) *Reader { return &Reader{rc: rc, r: r} }

func (x *Reader) Read(p []byte) (int, error) {
	n, err := x.rc.Read(p)
	x.lines.Feed(p[:n], x.r, x.r.Incoming)
	return n, err
}

func (x *Reader) Close() error { return x.rc.Close() }
