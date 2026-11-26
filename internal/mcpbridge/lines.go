package mcpbridge

import (
	"bytes"
	"io"
	"sync"
)

// MaxObserved bounds the bytes kept of one message while reading it; a longer message's content is
// omitted, while its bounded protocol envelope still acknowledges a successful write.
const MaxObserved = 1 << 20

// Lines splits a stream into lines. A line is kept only when a request awaits its response at
// the line's start, so idle traffic is never buffered.
type Lines struct {
	buf      []byte
	started  bool
	keep     bool
	overflow bool
	envelope Envelope
	sse      bool
}

// Feed passes each complete kept line, without its line ending, to line.
func (s *Lines) Feed(p []byte, r *Responses, line func([]byte)) {
	for len(p) > 0 {
		if !s.started {
			s.started, s.keep, s.overflow, s.buf = true, r.Waiting(), false, s.buf[:0]
			s.envelope = Envelope{}
			s.sse = false
		}
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if s.keep {
			s.envelope.Feed(chunk)
		}
		if s.keep && !s.overflow {
			if len(s.buf)+len(chunk) > MaxObserved {
				// The line so far: an SSE prefix may be split across writes.
				head := append(s.buf[:min(len(s.buf), 5):min(len(s.buf), 5)], chunk[:min(len(chunk), 5)]...)
				s.sse = bytes.HasPrefix(head, []byte("data:"))
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
		if s.keep && s.overflow {
			if envelope := s.envelope.JSON(); len(envelope) > 0 {
				if s.sse {
					envelope = append([]byte("data: "), envelope...)
				}
				guard(func() { line(envelope) })
			}
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

func NewWriter(w io.Writer, r *Responses) *Writer { return &Writer{w: w, r: r} }

func (x *Writer) Write(p []byte) (int, error) {
	var lines [][]byte
	var n int
	var err error
	func() {
		x.mu.Lock()
		defer x.mu.Unlock()
		n, err = x.w.Write(p)
		attempted := p[:n]
		if err != nil {
			attempted = p
		}
		x.lines.Feed(attempted, x.r, func(line []byte) { lines = append(lines, bytes.Clone(line)) })
	}()
	// Observer releases may invoke application callbacks; never hold the writer lock.
	for _, line := range lines {
		if err != nil {
			x.r.FailJSON(line)
		} else {
			x.r.DeliverJSON(line)
		}
	}
	if err != nil {
		// The connection is broken: release every other waiter without a response.
		x.r.Close()
	}
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

func (x *Reader) Close() error {
	x.r.Close()
	return x.rc.Close()
}
