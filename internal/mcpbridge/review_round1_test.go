package mcpbridge

import (
	"testing"
	"time"
)

type review84PanicWriter struct{ calls int }

func (w *review84PanicWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		panic("application writer panic")
	}
	return len(p), nil
}
func TestReview84WriterPanicUnlock(t *testing.T) {
	w := NewWriter(&review84PanicWriter{}, NewResponses())
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected underlying panic")
			}
		}()
		_, _ = w.Write([]byte("first\n"))
	}()
	done := make(chan struct{})
	go func() { _, _ = w.Write([]byte("second\n")); close(done) }()
	select {
	case <-done:
		t.Log("second write completed after recovering first panic")
	case <-time.After(100 * time.Millisecond):
		t.Error("second write deadlocked: writer mutex retained after panic")
	}
}
