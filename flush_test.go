package rudderstackai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// ctxClient is an analytics-go client with Flush(ctx).
type ctxClient struct {
	sink
	ctxFlushes atomic.Int32
	block      bool
}

func (c *ctxClient) Flush(ctx context.Context) error {
	c.ctxFlushes.Add(1)
	if c.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// oldClient is analytics-go before Flush(ctx): Enqueue and Close only.
type oldClient struct{ sink }

func (*oldClient) Flush() {} // not Flusher: no error result

func TestFlushUsesEachClientsFlush(t *testing.T) {
	seen := &seenErrors{}
	modern, legacy, old := &ctxClient{}, &sink{}, &oldClient{}
	setupAI(t, WithOnError(seen.hook), WithAnalytics(modern))
	for _, c := range []analytics.Client{legacy, old} {
		emitAI(c, "ai_user_action", "u", "", map[string]any{}, nil, "", time.Now())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !Flush(ctx) {
		t.Fatal("Flush timed out")
	}
	if modern.ctxFlushes.Load() != 1 || legacy.flushes.Load() != 1 {
		t.Fatalf("flushes: ctx %d, legacy %d", modern.ctxFlushes.Load(), legacy.flushes.Load())
	}
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindClientOutdated, Where: "Flush"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestFlushSharesOneDeadline(t *testing.T) {
	seen := &seenErrors{}
	stuck := &ctxClient{block: true}
	setupAI(t, WithOnError(seen.hook), WithAnalytics(stuck))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if Flush(ctx) {
		t.Fatal("Flush reported success past its deadline")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Flush took %v", d)
	}
	if got := seen.all(); len(got) != 1 || got[0].Kind != KindFlushTimeout {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestFlushTimeoutAlias(t *testing.T) {
	c := &ctxClient{}
	setupAI(t, WithAnalytics(c))
	if !FlushTimeout(time.Second) || c.ctxFlushes.Load() != 1 {
		t.Fatal("FlushTimeout did not flush")
	}
}

func TestShutdownEndsOpenInteractionsAndStopsBackgroundWork(t *testing.T) {
	var classified atomic.Int32
	c := &ctxClient{}
	setupAI(t, WithAnalytics(c), WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) {
		classified.Add(1)
		return ClassifyResult{Intent: "x"}, nil
	}}))
	_, a := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	a.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 2})
	_, b := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	if !Shutdown(context.Background()) {
		t.Fatal("Shutdown timed out")
	}
	if !Shutdown(context.Background()) {
		t.Fatal("a second Shutdown failed")
	}
	if !a.Ended() || !b.Ended() || openCount() != 0 {
		t.Fatal("open interactions left open")
	}
	got := c.named("ai_response_received")
	if len(got) != 2 {
		t.Fatalf("events %v", c.names())
	}
	for _, e := range got {
		if e.Properties["response_status"] != "incomplete" || e.Properties["end_reason"] != "shutdown" {
			t.Fatalf("shutdown end: %v", e.Properties)
		}
	}
	a.End(EndOptions{}) // its own end sends nothing more
	answer(t, InteractionOptions{PromptText: "p"}, "a")
	Flush(context.Background())
	if len(c.named("ai_response_received")) != 3 || classified.Load() != 0 || c.ctxFlushes.Load() < 2 {
		t.Fatalf("events %v, classified %d, flushes %d", c.names(), classified.Load(), c.ctxFlushes.Load())
	}
}

func TestRunTaskFlushIsBounded(t *testing.T) {
	seen := &seenErrors{}
	stuck := &ctxClient{block: true}
	setupAI(t, WithOnError(seen.hook))
	start := time.Now()
	err := RunTask(context.Background(), "job", TaskOptions{Analytics: stuck, FlushTimeout: 50 * time.Millisecond}, func(context.Context, *Interaction) error {
		return errors.New("task failed")
	})
	if err == nil || err.Error() != "task failed" {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second || stuck.ctxFlushes.Load() != 1 {
		t.Fatalf("task flush took %v (%d flushes)", d, stuck.ctxFlushes.Load())
	}
	// A flush that runs out of time is reported, as in Node; one that finishes is not.
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindFlushTimeout, Where: "RunTask"}) {
		t.Fatalf("hook saw %+v", got)
	}
	_ = RunTask(context.Background(), "job", TaskOptions{Analytics: &ctxClient{}}, func(context.Context, *Interaction) error { return nil })
	if got := seen.all(); len(got) != 1 {
		t.Fatalf("hook saw %+v after a finished flush", got[1:])
	}
}

// burst runs n interactions at once, each with one model call, against client.
func burst(n int, client analytics.Client) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u", PromptText: "hello", Analytics: client}, func(_ context.Context, i *Interaction) error {
				i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1-mini", InputTokens: 120, OutputTokens: 80, ResponseID: i.ID()})
				return nil
			})
		})
	}
	wg.Wait()
}

type nopClient struct{}

func (nopClient) Enqueue(analytics.Message) error { return nil }
func (nopClient) Close() error                    { return nil }

// The SDK's own work grows linearly with a burst: 6.5 times the interactions take well under 10
// times as long (the 7.5 s report for 1,300 interactions was the Node.js client's quadratic).
func TestBurstScalesLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	setupAI(t)
	best := func(n int) time.Duration {
		var b time.Duration
		for range 5 {
			start := time.Now()
			burst(n, nopClient{})
			if d := time.Since(start); b == 0 || d < b {
				b = d
			}
		}
		return b
	}
	burst(1300, nopClient{}) // warm up
	small, large := best(200), best(1300)
	ratio := float64(large) / float64(small)
	t.Logf("200: %v, 1300: %v (%.1fx)", small, large, ratio)
	if ratio > 10 {
		t.Fatal("the burst grows faster than linearly")
	}
}

// BenchmarkBurst1300 is the reported scenario: 1,300 concurrent interactions (prompt event, one
// model call, answer event each), SDK only.
func BenchmarkBurst1300(b *testing.B) {
	resetForTest()
	b.Cleanup(resetForTest)
	for b.Loop() {
		burst(1300, nopClient{})
	}
}

// BenchmarkBurst1300AnalyticsGo is the same burst through a real analytics-go client (its
// 100-slot queue, batching and HTTP against a local server), closed after each burst.
func BenchmarkBurst1300AnalyticsGo(b *testing.B) {
	resetForTest()
	b.Cleanup(resetForTest)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	for b.Loop() {
		client, err := analytics.NewWithConfig("key", analytics.Config{DataPlaneUrl: srv.URL, BatchSize: 100})
		if err != nil {
			b.Fatal(err)
		}
		burst(1300, client)
		_ = client.Close()
	}
}
