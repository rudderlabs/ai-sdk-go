package rudderstackai

import (
	"context"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP Events is a draft MCP extension: an agent subscribes to server updates, delivered by
// signed webhook. The go-sdk has no hooks for it, so these experimental helpers wrap the
// server's own subscription handling and webhook sender. They may change with the draft.

// Subscription actions for [SubscriptionChange.Action].
const (
	SubscriptionCreated        = "created"
	SubscriptionRefreshed      = "refreshed"
	SubscriptionUnsubscribed   = "unsubscribed"
	SubscriptionRevoked        = "revoked"
	SubscriptionExpired        = "expired"
	SubscriptionCallbackFailed = "callback_failed"
)

// Delivery statuses for [DeliveryOutcome.DeliveryStatus].
const (
	DeliveryAcknowledged = "acknowledged"
	DeliveryRejected     = "rejected"
	DeliveryGaveUp       = "gave_up"
)

// EventsIdentity is who an MCP Events record is for: explicit IDs, or a token passed to
// [Options.Identify]. Explicit IDs win over what Identify returns.
type EventsIdentity struct {
	// Token is the subscriber's verified bearer token, passed to Options.Identify. Nil without
	// auth.
	Token *auth.TokenInfo
	// UserID is the subscriber, sent as userId.
	UserID string
	// AccountID is the subscriber's workspace or tenant, sent as account_id.
	AccountID string
}

// SubscriptionChange is one MCP Events subscription change. SubscriptionID and EventName are
// required: a change without them is not sent.
type SubscriptionChange struct {
	EventsIdentity
	// Action is one of the Subscription* constants.
	Action string
	// SubscriptionID is the protocol's subscription ID.
	SubscriptionID string
	// EventName is the subscribed update, for example "comment.created".
	EventName string
	// Arguments are the subscription's filter arguments: only their names are sent.
	Arguments map[string]any
	// DeliveryMode is, for example, "webhook".
	DeliveryMode string
	// TTL is the granted lifetime. On created and refreshed, an expired change is sent when it
	// passes without a refresh, unsubscribe or revoke in this process. Zero means none.
	TTL time.Duration
	// ResumedFromCursor reports a subscription resumed from a client's cursor; nil leaves it out.
	ResumedFromCursor *bool
	// HistoryTruncated reports that events before the cursor were dropped; nil leaves it out.
	HistoryTruncated *bool
	// FailureReason is for callback_failed, for example "challenge_failed" or "timeout".
	FailureReason string
	// SessionID is the MCP session the change was made in, if any.
	SessionID string
	// ClientName is the subscribing client's name, if known.
	ClientName string
}

// EventDelivery is one update to deliver, as passed to an [EventSender]. SubscriptionID,
// EventName and EventID are required: a delivery without them is not recorded.
type EventDelivery struct {
	EventsIdentity
	SubscriptionID string
	EventName      string
	// EventID is the protocol event ID, kept across retries.
	EventID string
	// Payload is the body to deliver: only its size is sent.
	Payload any
	// OccurredAt is when the update happened, for delivery_latency_ms; zero leaves it out.
	OccurredAt time.Time
}

// DeliveryOutcome is what a sender reports after its last attempt.
type DeliveryOutcome struct {
	// HTTPStatus is the final status: 2xx acknowledges, 410 and 413 are rejections, others
	// give up.
	HTTPStatus int
	// AttemptCount is the deliveries tried, including retries.
	AttemptCount int
	// DeliveryStatus overrides the status derived from HTTPStatus.
	DeliveryStatus string
}

// EventSender delivers one update, with its own retries.
type EventSender func(ctx context.Context, d EventDelivery) (DeliveryOutcome, error)

func eventsTarget(server *mcp.Server) *wrapped {
	w, ok := instances.get(server)
	if !ok {
		debugf("mcp events: server not wrapped; nothing sent")
		return nil
	}
	return w
}

func (w *wrapped) eventsBase(ctx context.Context, e EventsIdentity) (*options, who, map[string]any) {
	opts := w.opts.Load()
	id := opts.identity(ctx, e.Token)
	base := map[string]any{"server_name": opts.name}
	account := capID(e.AccountID)
	if account == "" {
		account = id.AccountID
	}
	put(base, "account_id", account)
	user := capID(e.UserID)
	if user == "" {
		user = id.UserID
	}
	return opts, who{userID: user, anonymousID: id.AnonymousID}, base
}

// TrackMCPEventSubscription records an MCP Events subscription change as
// mcp_event_subscription_changed. Call it from your subscribe and unsubscribe handlers and
// wherever the server revokes a subscription. Needs [Options.MCPEvents]. Experimental.
func TrackMCPEventSubscription(ctx context.Context, server *mcp.Server, change SubscriptionChange) {
	safely(func() {
		if w := eventsTarget(server); w != nil {
			w.TrackSubscription(ctx, change)
		}
	})
}

// TrackSubscription records one subscription change for this server.
func (w *wrapped) TrackSubscription(ctx context.Context, c any) {
	change, ok := c.(SubscriptionChange)
	if !ok || !w.opts.Load().MCPEvents {
		return
	}
	id, event := capID(change.SubscriptionID), capID(change.EventName)
	if id == "" || event == "" {
		return
	}
	opts, whom, base := w.eventsBase(ctx, change.EventsIdentity)
	p := map[string]any{
		"subscription_action": change.Action,
		"subscription_id":     id,
		"event_name":          event,
	}
	if change.Arguments != nil {
		keys := make([]string, 0, len(change.Arguments))
		for k := range change.Arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		p["argument_keys"] = keys
	}
	put(p, "delivery_mode", capLabel(change.DeliveryMode))
	if change.TTL > 0 {
		p["ttl_ms"] = change.TTL.Milliseconds()
	}
	if change.ResumedFromCursor != nil {
		p["resumed_from_cursor"] = *change.ResumedFromCursor
	}
	if change.HistoryTruncated != nil {
		p["history_truncated"] = *change.HistoryTruncated
	}
	put(p, "failure_reason", capLabel(change.FailureReason))
	put(p, "mcp_session_id", capID(change.SessionID))
	put(p, "client_name", capID(change.ClientName))
	for k, v := range base {
		p[k] = v
	}
	emit(opts.Analytics, "mcp_event_subscription_changed", whom, p)

	st := w.state()
	st.Lock()
	defer st.Unlock()
	if t, ok := st.expiries.get(id); ok {
		t.Stop()
		st.expiries.remove(id)
	}
	if (change.Action == SubscriptionCreated || change.Action == SubscriptionRefreshed) && change.TTL > 0 {
		expired := change
		expired.Action = SubscriptionExpired
		expired.TTL = 0
		expired.ResumedFromCursor = nil
		expired.HistoryTruncated = nil
		var t *time.Timer
		t = time.AfterFunc(change.TTL, func() {
			st.Lock()
			if current, ok := st.expiries.get(id); !ok || current != t {
				st.Unlock()
				return
			}
			st.expiries.remove(id)
			st.Unlock()
			safely(func() { w.TrackSubscription(context.WithoutCancel(ctx), expired) })
		})
		st.expiries.set(id, t)
	}
}

func deliveryStatus(o DeliveryOutcome, failed bool) string {
	switch {
	case failed:
		return DeliveryGaveUp
	case o.DeliveryStatus != "":
		return o.DeliveryStatus
	case o.HTTPStatus >= 200 && o.HTTPStatus < 300:
		return DeliveryAcknowledged
	case o.HTTPStatus == 410 || o.HTTPStatus == 413:
		return DeliveryRejected
	default:
		return DeliveryGaveUp
	}
}

func payloadSize(v any) int {
	switch p := v.(type) {
	case string:
		return len(p)
	case []byte:
		return len(p)
	default:
		return byteSize(p)
	}
}

// WrapMCPEventSender wraps your webhook sender so each finished delivery (after its retries) is
// recorded as mcp_event_delivered. The sender keeps its result; if it returns an error, the
// delivery is gave_up. Never the payload, only its size. Needs [Options.MCPEvents].
// Experimental.
func WrapMCPEventSender(server *mcp.Server, sender EventSender) EventSender {
	return func(ctx context.Context, d EventDelivery) (DeliveryOutcome, error) {
		outcome, err := sender(ctx, d)
		safely(func() {
			if w := eventsTarget(server); w != nil {
				w.DeliveryDone(ctx, d, outcome, err != nil)
			}
		})
		return outcome, err
	}
}

// DeliveryDone records one finished delivery for this server.
func (w *wrapped) DeliveryDone(ctx context.Context, delivery, result any, failed bool) {
	d, ok := delivery.(EventDelivery)
	outcome, _ := result.(DeliveryOutcome)
	if !ok || !w.opts.Load().MCPEvents {
		return
	}
	id, event, eventID := capID(d.SubscriptionID), capID(d.EventName), capID(d.EventID)
	if id == "" || event == "" || eventID == "" {
		return
	}
	opts, whom, base := w.eventsBase(ctx, d.EventsIdentity)
	p := map[string]any{
		"subscription_id": id,
		"event_name":      event,
		"event_id":        eventID,
		"delivery_status": deliveryStatus(outcome, failed),
	}
	if !failed && outcome.HTTPStatus != 0 {
		p["http_status"] = outcome.HTTPStatus
	}
	if !failed && outcome.AttemptCount >= 1 {
		p["attempt_count"] = outcome.AttemptCount
	}
	if p["delivery_status"] == DeliveryAcknowledged && !d.OccurredAt.IsZero() {
		p["delivery_latency_ms"] = max(0, time.Since(d.OccurredAt).Milliseconds())
	}
	if d.Payload != nil {
		p["payload_size_bytes"] = payloadSize(d.Payload)
	}
	for k, v := range base {
		p[k] = v
	}
	emit(opts.Analytics, "mcp_event_delivered", whom, p)
}
