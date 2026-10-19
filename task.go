package rudderstackai

import (
	"context"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
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
	// functions that exit right after (when the client implements Flush() error). Default: on,
	// except for a task that chains to the interaction in its context.
	Flush *bool
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
		// An opted-in classification goes out with the flush (bounded by its timeout).
		task.waitBackground(defaultClassifyTimeout + time.Second)
		safely(func() { flushClient(task.analytics()) })
	}
	return err
}

func flushClient(c analytics.Client) {
	if f, ok := c.(Flusher); ok {
		if err := f.Flush(); err != nil {
			debugf("analytics flush failed (ignored)", "error", err)
		}
	}
}

// Flush waits for pending classifications and scores (at most timeout; zero means the classifier
// timeout plus a second), then flushes every RudderStack client the SDK has used that implements
// [Flusher]. Call it before a script, worker or serverless handler exits; analytics-go clients
// send their queue on Close. It reports whether every classification finished.
func Flush(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultClassifyTimeout + time.Second
		if c := cfg().classifier; c != nil {
			timeout = c.Timeout + time.Second
		}
	}
	finished := waitBackgroundWork(timeout)
	global.mu.RLock()
	clients := append([]analytics.Client(nil), global.clients...)
	if global.cfg.analytics != nil {
		clients = append(clients, global.cfg.analytics)
	}
	global.mu.RUnlock()
	for _, c := range clients {
		safely(func() { flushClient(c) })
	}
	return finished
}
