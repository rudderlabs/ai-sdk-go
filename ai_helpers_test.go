package rudderstackai

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// setupAI restores the defaults and configures a schema-validating recorder as the global client.
func setupAI(t *testing.T, opts ...ConfigOption) *recorder {
	t.Helper()
	waitBackgroundWork(10 * time.Second)
	resetForTest()
	r := newRecorder(t)
	if err := Configure(append([]ConfigOption{WithAnalytics(r)}, opts...)...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		waitBackgroundWork(10 * time.Second)
		resetForTest()
	})
	return r
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// count is how many log lines contain s.
func (l *logBuffer) count(s string) int { return strings.Count(l.String(), s) }

// captureLogs sends slog warnings to a buffer for the rest of the test.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	l := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// sink is a client that keeps events without schema checks, can fail, and counts flushes.
type sink struct {
	mu       sync.Mutex
	events   []analytics.Track
	err      error
	panics   bool
	flushes  atomic.Int32
	flushErr error
}

func (s *sink) Enqueue(m analytics.Message) error {
	if s.panics {
		panic("client exploded")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := m.(analytics.Track); ok {
		s.events = append(s.events, t)
	}
	return s.err
}

func (s *sink) Close() error { return nil }

func (s *sink) Flush() error {
	s.flushes.Add(1)
	return s.flushErr
}

func (s *sink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e.Event)
	}
	return out
}

func (s *sink) named(event string) []analytics.Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []analytics.Track
	for _, e := range s.events {
		if e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

// sliceClient is an uncomparable client.
type sliceClient []int

func (sliceClient) Enqueue(analytics.Message) error { return nil }
func (sliceClient) Close() error                    { return nil }

// response returns the only ai_response_received.
func (r *recorder) response(t *testing.T) map[string]any {
	t.Helper()
	return r.props(t, "ai_response_received")
}

// integrationOf is the context.wrapperLibrary.integration of a message.
func integrationOf(m analytics.Track) string {
	w, _ := m.Context.Extra["wrapperLibrary"].(map[string]any)
	s, _ := w["integration"].(string)
	return s
}

// run is RunInteraction with a fixed user and no error.
func run(t *testing.T, o InteractionOptions, fn func(ctx context.Context, i *Interaction)) {
	t.Helper()
	if o.UserID == "" && o.AnonymousID == "" {
		o.UserID = "u1"
	}
	if err := RunInteraction(context.Background(), o, func(ctx context.Context, i *Interaction) error {
		fn(ctx, i)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
