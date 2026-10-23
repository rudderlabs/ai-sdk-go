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
	InteractionID  string
	Feature        string
	Surface        Surface
	TraceID        string
	// AccountID is the customer, workspace or tenant the work is for.
	AccountID string
	// OnBehalfOf is the human the work is for. The task's actor is always the system.
	OnBehalfOf string
	// TriggerSource is why the run started: "schedule", "webhook", "queue"... Defaults to
	// "agent" when the task has a parent.
	TriggerSource  string
	TriggerEventID string
	// ParentInteractionID is the interaction that started the task. Defaults to the
	// interaction in the context (sub-agents chain automatically).
	ParentInteractionID string
	// NoParent opts out of chaining to the interaction in the context.
	NoParent       bool
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
	// reported as flush_timeout.
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
		UserID: o.UserID, AnonymousID: o.AnonymousID, ConversationID: conversation, InteractionID: o.InteractionID, Feature: o.Feature,
		Surface: o.Surface, TraceID: o.TraceID, AccountID: o.AccountID, OnBehalfOf: o.OnBehalfOf,
		TriggerSource: triggerSource, TriggerEventID: o.TriggerEventID, TaskName: taskName,
		ParentInteractionID: parent, Analytics: o.Analytics, CaptureContent: o.CaptureContent,
		ToolBreakdown: o.ToolBreakdown, Classify: o.Classify, Context: o.Context,
		Trigger: TriggerSystem, NoPromptEvent: true, IdentityFromCookies: Ptr(false),
	}, func(ctx context.Context, i *Interaction) error {
		task = i
		return fn(ctx, i)
	})
	if flush && task != nil {
		// An opted-in classification goes out with the flush, all within one bounded wait.
		wait := cmp.Or(o.FlushTimeout, taskFlushTimeout)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
		defer cancel()
		finished := true
		safely(func() {
			finished = task.waitBackground(ctx) && flushClients(ctx, []analytics.Client{task.analytics()})
		})
		if !finished {
			report(KindFlushTimeout, "RunTask", ctx.Err())
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
// deadline: analytics-go from the release that adds Flush(ctx).
type ContextFlusher interface {
	Flush(ctx context.Context) error
}

// flushClients flushes each client in parallel within ctx; false when ctx ended first. A client
// that cannot flush (analytics-go before Flush(ctx)) is reported as client_outdated: its events
// go out on its own schedule or at Close.
func flushClients(ctx context.Context, clients []analytics.Client) bool {
	var wg sync.WaitGroup
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
				if err := flush(); err != nil {
					debugf("analytics flush failed (ignored)", "error", err)
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
		return true
	case <-ctx.Done():
		return false
	}
}

var errNoClientFlush = errors.New("rudderstackai: the RudderStack client has no Flush(ctx); upgrade analytics-go, or Close the client before the process exits")

// Flush sends what the SDK holds, within ctx (10 seconds when ctx has no deadline): it waits for
// pending classifications, scores and failed provider attempts held for a retry, then flushes
// every RudderStack client the SDK has used, in parallel. A client implementing
// [ContextFlusher] (analytics-go from the release with Flush(ctx)) sends its whole queue; one
// implementing [Flusher] is flushed as before; an older analytics-go client cannot be flushed
// (reported as client_outdated), so Close it before the process exits. Flush never panics,
// never ends open interactions and never closes a client. It reports false, and flush_timeout,
// when ctx ended first.
//
//	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
//	defer cancel()
//	rudderstackai.Flush(ctx)
func Flush(ctx context.Context) (finished bool) {
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
		finished = flushClients(ctx, clients) && finished
	})
	if !finished {
		report(KindFlushTimeout, "Flush", ctx.Err())
	}
	return finished
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
