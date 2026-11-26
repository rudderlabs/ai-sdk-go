package kit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// cancelStream opens a spanned stream over src whose cancellation is settled on the context's
// goroutine.
func cancelStream(ctx context.Context, t *testing.T, h StreamHandler, src io.ReadCloser) *http.Response {
	t.Helper()
	spec := Spec{Provider: "openai", Integration: "review",
		IsStream:  func(*http.Request, map[string]any) bool { return true },
		NewStream: func(*Call, *http.Response) StreamHandler { return h }}
	resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: src}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Body.(*sseBody); !ok {
		t.Fatal("stream not observed")
	}
	return resp
}

// openEvent is a stream that sends one event and stays open until closed.
func openEvent(t *testing.T) io.ReadCloser {
	t.Helper()
	r, w := io.Pipe()
	go func() { _, _ = w.Write([]byte("data: {}\n\n")) }()
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func cancellable(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	reviewRecorder(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	t.Cleanup(func() { i.End(rudderstackai.EndOptions{}) })
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	return ctx, cancel
}

// closeAfterReport closes the stream once the cancellation report is running and requires
// Close to return only after release.
func closeAfterReport(t *testing.T, resp *http.Response, reporting, release chan struct{}) {
	t.Helper()
	<-reporting
	closed := make(chan struct{})
	go func() { defer close(closed); _ = resp.Body.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned before the cancellation was reported")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the report")
	}
}

// SDK-7211: the app's Close after a cancel must not return while the cancellation goroutine is
// still reporting, or a tracing twin and End that follow it would beat the cancelled call.
func TestCloseWaitsForCancellationReport(t *testing.T) {
	ctx, cancel := cancellable(t)
	reporting, release := make(chan struct{}), make(chan struct{})
	resp := cancelStream(ctx, t, &reentryHandler{finish: func() { close(reporting); <-release }}, io.NopCloser(strings.NewReader("data: {}\n\n")))
	cancel()
	closeAfterReport(t, resp, reporting, release)
}

// A cancel during a reader's callback is still reported by the cancellation goroutine, so a
// Close from another goroutine waits for it.
func TestCloseWaitsForCancellationDuringReaderCallback(t *testing.T) {
	ctx, cancel := cancellable(t)
	inEvent, releaseEvent := make(chan struct{}), make(chan struct{})
	reporting, release := make(chan struct{}), make(chan struct{})
	resp := cancelStream(ctx, t, &reentryHandler{
		event:  func() { close(inEvent); <-releaseEvent },
		finish: func() { close(reporting); <-release },
	}, openEvent(t))
	read := make(chan struct{})
	go func() { defer close(read); _, _ = io.ReadAll(resp.Body) }()
	<-inEvent
	cancel()
	// Let the cancellation goroutine reach the reader's running drain.
	time.Sleep(20 * time.Millisecond)
	close(releaseEvent)
	closeAfterReport(t, resp, reporting, release)
	<-read
}

// A reader's callback can close the stream while the cancellation goroutine waits for that
// reader: no deadlock, and the call is reported once.
func TestReaderCallbackClosesWhileCancellationWaits(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			ctx, cancel := cancellable(t)
			var finishes atomic.Int32
			h := &reentryHandler{finish: func() { finishes.Add(1) }}
			resp := cancelStream(ctx, t, h, openEvent(t))
			h.event = func() {
				cancel()
				time.Sleep(20 * time.Millisecond)
				if !threaded {
					_ = resp.Body.Close()
					return
				}
				closed := make(chan struct{})
				go func() { defer close(closed); _ = resp.Body.Close() }()
				<-closed
			}
			read := make(chan struct{})
			go func() { defer close(read); _, _ = io.ReadAll(resp.Body) }()
			select {
			case <-read:
			case <-time.After(2 * time.Second):
				t.Fatal("Close in a reader callback deadlocked with the cancellation")
			}
			time.Sleep(50 * time.Millisecond) // a second report would land by now
			if n := finishes.Load(); n != 1 {
				t.Fatalf("reports=%d", n)
			}
		})
	}
}

// The cancellation report can run app code that closes the stream: on its own goroutine without
// waiting, or on one it waits for within the cap.
func TestCancellationReportReentersClose(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			if threaded {
				defer func(wait time.Duration) { cancelReportWait = wait }(cancelReportWait)
				cancelReportWait = 50 * time.Millisecond
			}
			ctx, cancel := cancellable(t)
			h := &reentryHandler{}
			resp := cancelStream(ctx, t, h, io.NopCloser(strings.NewReader("data: {}\n\n")))
			reported := make(chan struct{})
			h.finish = func() {
				defer close(reported)
				if !threaded {
					_ = resp.Body.Close()
					return
				}
				closed := make(chan struct{})
				go func() { defer close(closed); _ = resp.Body.Close() }()
				<-closed
			}
			cancel()
			select {
			case <-reported:
			case <-time.After(time.Second):
				t.Fatal("Close inside the cancellation report blocked")
			}
		})
	}
}

// No cancellation report pending: Close does not wait, even while the reader is in a callback.
func TestCloseDoesNotWaitWithoutCancellation(t *testing.T) {
	ctx, _ := cancellable(t)
	reading, release := make(chan struct{}), make(chan struct{})
	resp := cancelStream(ctx, t, &reentryHandler{event: func() { close(reading); <-release }}, io.NopCloser(strings.NewReader("data: {}\n\n")))
	read := make(chan struct{})
	go func() { defer close(read); _, _ = io.ReadAll(resp.Body) }()
	<-reading
	closed := make(chan struct{})
	go func() { defer close(closed); _ = resp.Body.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited without a cancellation report")
	}
	close(release)
	<-read
}
