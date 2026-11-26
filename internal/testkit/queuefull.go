package testkit

import (
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// queueFullWait bounds each wait so an SDK bug fails the test instead of hanging it.
const queueFullWait = 10 * time.Second

// QueueFull is a client that accepts capacity events, then blocks in Enqueue until Run makes
// room, as analytics-go's Enqueue does on a full queue: it waits and never refuses the event.
type QueueFull struct {
	capacity int
	blocked  chan struct{}
	release  chan struct{}
	freed    sync.Once
	mu       sync.Mutex
	names    []string
}

// NewQueueFull returns a client with room for capacity events.
func NewQueueFull(capacity int) *QueueFull {
	return &QueueFull{capacity: capacity, blocked: make(chan struct{}, 1), release: make(chan struct{})}
}

// Enqueue implements [analytics.Client].
func (q *QueueFull) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return nil
	}
	q.mu.Lock()
	full := len(q.names) >= q.capacity
	if !full {
		q.names = append(q.names, t.Event) // under the same lock, so concurrent sends can't overfill
	}
	q.mu.Unlock()
	if !full {
		return nil
	}
	select {
	case q.blocked <- struct{}{}:
	default:
	}
	<-q.release
	q.mu.Lock()
	q.names = append(q.names, t.Event)
	q.mu.Unlock()
	return nil
}

// Close implements [analytics.Client].
func (q *QueueFull) Close() error { return nil }

// free makes room once; also run at cleanup so a failed test leaves no goroutine blocked.
func (q *QueueFull) free() { q.freed.Do(func() { close(q.release) }) }

// Run runs path in a goroutine. When it sends more than capacity events, Run waits for an
// Enqueue to block, then makes room. It waits for path to finish and returns the accepted
// event names in order.
func (q *QueueFull) Run(t *testing.T, sends int, path func()) []string {
	t.Helper()
	t.Cleanup(q.free)
	done := make(chan struct{})
	go func() {
		defer close(done)
		path()
	}()
	if sends > q.capacity {
		select {
		case <-q.blocked:
		case <-done:
			t.Fatalf("path sent %d events past capacity %d without blocking", sends, q.capacity)
		case <-time.After(queueFullWait):
			t.Fatal("no Enqueue blocked on the full queue")
		}
	}
	q.free()
	select {
	case <-done:
	case <-time.After(queueFullWait):
		t.Fatal("path did not finish after the queue had room")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.names...)
}
