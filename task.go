package rudderstackai

import (
	"cmp"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// TaskOptions configures [RunTask].
type TaskOptions struct {
	// UserID and AnonymousID identify the actor when it is not the system; never inherited from
	// the context.
	UserID      string
	AnonymousID string
	// ConversationID defaults to the parent interaction's; otherwise one is minted.
	ConversationID string
	// OneShot uses the interaction ID when there is no conversation. Since v0.4; default false.
	OneShot       bool
	InteractionID string
	Feature       string
	Surface       Surface
	TraceID       string
	// AccountID is the customer, workspace or tenant the work is for.
	AccountID string
	// OnBehalfOf is the human the work is for. The task's actor is always the system.
	OnBehalfOf string
	// AgentVersion names the app release or configuration; Variant names its experiment arm.
	AgentVersion string
	Variant      string
	// TriggerSource is why the run started: "schedule", "webhook", "queue"... Defaults to
	// "agent" when the task has a parent.
	TriggerSource  string
	TriggerEventID string
	// ParentInteractionID is the interaction that started the task. Defaults to the
	// interaction in the context (sub-agents chain automatically).
	ParentInteractionID string
	// NoParent opts out of chaining to the interaction in the context.
	NoParent bool
	// Resume continues a paused run from its snapshot ([InteractionOptions.Resume]); no
	// ai_task_started is sent.
	Resume         *PausedInteraction
	Analytics      analytics.Client
	CaptureContent *bool
	ToolBreakdown  *bool
	// Classify classifies the task when a classifier is configured. Off by default.
	Classify *bool
	Context  map[string]any
	// Flush flushes the analytics client when the task ends, for workers and serverless
	// functions that exit right after (as [Flush] does for one client). Default: on, except for a
	// task that chains to the interaction in its context.
	Flush *bool
	// FlushTimeout bounds that flush, classification included. Zero: 2 seconds. Running out is
	// reported as flush_timeout; the client's own flush error is passed, unchanged, as flush_failed.
	FlushTimeout time.Duration
}

// Flusher is implemented by RudderStack clients that can send queued events on demand.
type Flusher interface {
	Flush() error
}

// RunTask runs a background job (no UI: a schedule, webhook, queue or another agent) as one
// system-triggered interaction named taskName. An error returned by fn fails the answer and is
// returned unchanged.
//
// The task's actor is the system: user and anonymous IDs are never inherited from the context,
// and cookies are never read. Put the human the work is for in OnBehalfOf.
func RunTask(ctx context.Context, taskName string, o TaskOptions, fn func(context.Context, *Interaction) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	current := FromContext(ctx)
	chained := current
	if o.ParentInteractionID != "" || o.NoParent {
		chained = nil
	}
	parent := o.ParentInteractionID
	if parent == "" && chained != nil {
		parent = chained.id
	}
	if o.NoParent {
		parent = ""
	}
	flush := o.ParentInteractionID != "" || o.NoParent || current == nil || current.Ended()
	if o.Flush != nil {
		flush = *o.Flush
	}
	conversation := o.ConversationID
	if conversation == "" && chained != nil {
		conversation = chained.conversationID
	}
	triggerSource := o.TriggerSource
	if triggerSource == "" && parent != "" {
		triggerSource = "agent"
	}
	// A task acts as the system: identity and conversation come only from its own options.
	if cv := valuesFrom(ctx); cv != nil {
		c := *cv
		c.userID, c.anonymousID, c.conversationID, c.cookies = "", "", "", nil
		c.disconnected = nil // a task outlives the request that started it
		ctx = context.WithValue(ctx, keyValues, &c)
	}
	var task *Interaction
	err := RunInteraction(ctx, InteractionOptions{
		UserID: o.UserID, AnonymousID: o.AnonymousID, ConversationID: conversation, InteractionID: o.InteractionID, OneShot: o.OneShot, Feature: o.Feature,
		Surface: o.Surface, TraceID: o.TraceID, AccountID: o.AccountID, OnBehalfOf: o.OnBehalfOf, AgentVersion: o.AgentVersion, Variant: o.Variant,
		TriggerSource: triggerSource, TriggerEventID: o.TriggerEventID, TaskName: taskName,
		ParentInteractionID: parent, Analytics: o.Analytics, CaptureContent: o.CaptureContent,
		ToolBreakdown: o.ToolBreakdown, Classify: o.Classify, Context: o.Context,
		Trigger: TriggerSystem, NoPromptEvent: true, IdentityFromCookies: Ptr(false), Resume: o.Resume,
	}, func(ctx context.Context, i *Interaction) error {
		task = i
		i.taskStarted()
		return fn(ctx, i)
	})
	if flush && task != nil {
		// An opted-in classification goes out with the flush, all within one bounded wait.
		wait := cmp.Or(o.FlushTimeout, taskFlushTimeout)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
		defer cancel()
		finished := true
		var finish func() bool
		safely(func() {
			finished = task.waitBackground(ctx)
			var drained bool
			drained, finish = flushClients(ctx, "RunTask", []analytics.Client{task.analytics()})
			finished = drained && finished
		})
		if !finished {
			report(KindFlushTimeout, "RunTask", ctx.Err())
		}
		if finish != nil {
			finish()
		}
	}
	return err
}

const (
	// taskFlushTimeout bounds a task's own flush: classification and client together.
	taskFlushTimeout = 2 * time.Second
	// defaultFlushTimeout bounds Flush and Shutdown when ctx has no deadline.
	defaultFlushTimeout = 10 * time.Second
)

// ContextFlusher is implemented by RudderStack clients that send their queue on demand within a
// deadline: analytics-go v4.4.0 and later.
type ContextFlusher interface {
	Flush(ctx context.Context) error
}

// flushClients flushes each client in parallel within ctx; finished is false when ctx ended first
// (also when the flushes ended with it). The caller reports any timeout, then calls finish: it
// reports the first error a client's own Flush returned until then, unchanged, as flush_failed
// (at where), even after a timeout, since the app's own later Flush finds an empty queue, and is
// false when there was one. A flush that fails after finish is reported as flush_failed from its
// own goroutine. An error that is the expired ctx's own (the flush gave up at the deadline) is the
// timeout, never flush_failed. A client that cannot flush (a custom client, or analytics-go before
// v4.4.0) is reported as client_outdated: its events go out on its own schedule or at Close.
func flushClients(ctx context.Context, where string, clients []analytics.Client) (finished bool, finish func() bool) {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		first  error // written by flushes that may outlive ctx: read only under mu
		closed bool  // finish ran: later errors are reported from their goroutine
	)
	for _, c := range clients {
		var flush func() error
		switch f := c.(type) {
		case ContextFlusher:
			flush = func() error { return f.Flush(ctx) }
		case Flusher:
			flush = f.Flush
		case nil:
			continue
		default:
			report(KindClientOutdated, "Flush", errNoClientFlush)
			continue
		}
		wg.Go(func() {
			safely(func() {
				err := flush()
				if err == nil || timedOut(ctx, err) {
					return
				}
				mu.Lock()
				late := closed
				if !late && first == nil {
					first = err
				}
				mu.Unlock()
				if late {
					report(KindFlushFailed, where, err)
				}
			})
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// done and ctx.Done() can be ready together: flushes that ended with ctx did not finish in time
		finished = ctx.Err() == nil
	case <-ctx.Done():
	}
	return finished, func() bool {
		// under the lock, so a late failure cannot take flush_failed's rate-limit slot ahead of this one
		mu.Lock()
		defer mu.Unlock()
		closed = true
		if first != nil {
			report(KindFlushFailed, where, first)
		}
		return first == nil
	}
}

// timedOut reports whether err is ctx's own error once ctx has ended: a flush that gave up at the
// deadline, which is a timeout, not a client failure.
func timedOut(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	return ctxErr != nil && (errors.Is(err, ctxErr) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}

var errNoClientFlush = errors.New("rudderstackai: the RudderStack client has no Flush(ctx); use analytics-go v4.4.0 or later, or Close the client before the process exits")

// Flush sends what the SDK holds, within ctx (10 seconds when ctx has no deadline): it waits for
// pending classifications, scores and failed provider attempts held for a retry, then flushes
// every RudderStack client the SDK has used, in parallel. A client implementing
// [ContextFlusher] (analytics-go v4.4.0 and later) sends its whole queue; one implementing
// [Flusher] is flushed as before; a client with neither, such as a custom one, cannot be flushed
// (reported as client_outdated), so Close it before the process exits. Flush never panics,
// never ends open interactions and never closes a client. It reports false, and flush_timeout,
// when ctx ended first. Because it drains your client's queue, your own client.Flush afterwards
// has nothing left to fail on: when your client's flush fails, Flush returns false and passes your
// client's error, unchanged, to the [WithOnError] hook as flush_failed, so handle flush errors there.
// A deadline does not hide an error a client returned before it: both kinds are reported; one
// returned after Flush returned is reported then, from the flush's own goroutine.
//
//	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
//	defer cancel()
//	rudderstackai.Flush(ctx)
func Flush(ctx context.Context) (finished bool) {
	var finish func() bool
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultFlushTimeout)
		defer cancel()
	}
	safely(func() {
		finished = waitBackgroundUntil(ctx) && hook.WaitTracked(ctx)
		clients := rememberedClients()
		if c := cfg().analytics; c != nil {
			clients = append(clients, c)
		}
		var drained bool
		drained, finish = flushClients(ctx, "Flush", clients)
		finished = drained && finished
	})
	// both reported, whichever happened: a timeout does not hide an error a client returned before it
	if !finished {
		report(KindFlushTimeout, "Flush", ctx.Err())
	}
	ok := finish == nil || finish()
	return finished && ok
}

// FlushTimeout is Flush with a timeout (zero: 10 seconds).
//
// Deprecated: use [Flush] with a context.
func FlushTimeout(timeout time.Duration) bool {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return Flush(ctx)
}

// shuttingDown stops new background work (classification, evaluation) once Shutdown ran.
var shuttingDown atomic.Bool

// Shutdown prepares the process to exit: it stops queueing classifications and scores, ends
// every open interaction as response_status "incomplete" with end_reason "shutdown", then runs
// [Flush] within ctx and returns its result. It never closes a client: Close yours afterwards.
// Calling it again is safe. Call it from your SIGTERM handling; the SDK installs no signal or exit
// hooks.
func Shutdown(ctx context.Context) bool {
	shuttingDown.Store(true)
	safely(func() {
		for _, i := range takeOpen() {
			i.End(EndOptions{Status: StatusIncomplete, endReason: endReasonShutdown})
		}
	})
	return Flush(ctx)
}
