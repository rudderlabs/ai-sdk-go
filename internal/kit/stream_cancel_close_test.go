package kit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// cancelStream opens a spanned stream whose cancellation is settled on the context's goroutine.
func cancelStream(ctx context.Context, t *testing.T, h StreamHandler) *http.Response {
	t.Helper()
	spec := Spec{Provider: "openai", Integration: "review",
		IsStream:  func(*http.Request, map[string]any) bool { return true },
		NewStream: func(*Call, *http.Response) StreamHandler { return h }}
	resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Body.(*sseBody); !ok {
		t.Fatal("stream not observed")
	}
	return resp
}

// SDK-7211: the app's Close after a cancel must not return while the cancellation goroutine is
// still reporting, or a tracing twin and End that follow it would beat the cancelled call.
func TestCloseWaitsForCancellationReport(t *testing.T) {
	reviewRecorder(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	defer i.End(rudderstackai.EndOptions{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reporting, release := make(chan struct{}), make(chan struct{})
	resp := cancelStream(ctx, t, &reentryHandler{finish: func() { close(reporting); <-release }})
	cancel()
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

// The cancellation report can run app code that closes the stream, on its own goroutine or on
// one it waits for.
func TestCancellationReportReentersClose(t *testing.T) {
	defer func(wait time.Duration) { cancelReportWait = wait }(cancelReportWait)
	cancelReportWait = 50 * time.Millisecond
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			reviewRecorder(t)
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
			defer i.End(rudderstackai.EndOptions{})
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			h := &reentryHandler{}
			resp := cancelStream(ctx, t, h)
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
			case <-time.After(2 * time.Second):
				t.Fatal("Close inside the cancellation report deadlocked")
			}
		})
	}
}

// No cancellation report pending: Close does not wait, even while the reader is in a callback.
func TestCloseDoesNotWaitWithoutCancellation(t *testing.T) {
	reviewRecorder(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)})
	defer i.End(rudderstackai.EndOptions{})
	reading, release := make(chan struct{}), make(chan struct{})
	resp := cancelStream(ctx, t, &reentryHandler{event: func() { close(reading); <-release }})
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
