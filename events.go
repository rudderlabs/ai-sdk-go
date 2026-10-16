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
// [Options.Identify].
type EventsIdentity struct {
	Token     *auth.TokenInfo
	UserID    string
	AccountID string
}

// SubscriptionChange is one MCP Events subscription change.
type SubscriptionChange struct {
	EventsIdentity
	// Action is one of the Subscription* constants.
	Action         string
	SubscriptionID string
	// EventName is the subscribed update, for example "comment.created".
	EventName string
	// Arguments are the subscription's filter arguments: only their names are sent.
	Arguments map[string]any
	// DeliveryMode is, for example, "webhook".
	DeliveryMode string
	// TTL is the granted lifetime. On created and refreshed, an expired change is sent when it
	// passes without a refresh, unsubscribe or revoke in this process. Zero means none.
	TTL               time.Duration
	ResumedFromCursor *bool
	HistoryTruncated  *bool
	// FailureReason is for callback_failed, for example "challenge_failed" or "timeout".
	FailureReason string
	SessionID     string
	ClientName    string
}

// EventDelivery is one update to deliver, as passed to an [EventSender].
type EventDelivery struct {
	EventsIdentity
	SubscriptionID string
	EventName      string
	// EventID is the protocol event ID, kept across retries.
	EventID string
	// Payload is the body to deliver: only its size is sent.
	Payload any
	// OccurredAt is when the update happened, for delivery_latency_ms.
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
	v, ok := registry.Load(server)
	if !ok || !v.(*wrapped).opts.Load().MCPEvents {
		debugf("mcp events: server not wrapped with MCPEvents; nothing sent")
		return nil
	}
	return v.(*wrapped)
}

func (w *wrapped) eventsBase(ctx context.Context, e EventsIdentity) (*options, who, map[string]any) {
	opts := w.opts.Load()
	id := opts.identity(ctx, e.Token)
	base := map[string]any{"server_name": w.name}
	account := e.AccountID
	if account == "" {
		account = id.AccountID
	}
	put(base, "account_id", account)
	user := e.UserID
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
		w := eventsTarget(server)
		if w == nil || change.SubscriptionID == "" || change.EventName == "" {
			return
		}
		opts, whom, base := w.eventsBase(ctx, change.EventsIdentity)
		p := map[string]any{
			"subscription_action": change.Action,
			"subscription_id":     change.SubscriptionID,
			"event_name":          change.EventName,
		}
		if change.Arguments != nil {
			keys := make([]string, 0, len(change.Arguments))
			for k := range change.Arguments {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			p["argument_keys"] = keys
		}
		put(p, "delivery_mode", change.DeliveryMode)
		if change.TTL > 0 {
			p["ttl_ms"] = change.TTL.Milliseconds()
		}
		if change.ResumedFromCursor != nil {
			p["resumed_from_cursor"] = *change.ResumedFromCursor
		}
		if change.HistoryTruncated != nil {
			p["history_truncated"] = *change.HistoryTruncated
		}
		put(p, "failure_reason", change.FailureReason)
		put(p, "mcp_session_id", change.SessionID)
		put(p, "client_name", change.ClientName)
		for k, v := range base {
			p[k] = v
		}
		emit(opts.Analytics, "mcp_event_subscription_changed", whom, p)

		key := w.name + "\x00" + change.SubscriptionID
		w.mem.Lock()
		defer w.mem.Unlock()
		if t := w.mem.expiries[key]; t != nil {
			t.Stop()
			delete(w.mem.expiries, key)
		}
		if (change.Action == SubscriptionCreated || change.Action == SubscriptionRefreshed) && change.TTL > 0 {
			expired := change
			expired.Action = SubscriptionExpired
			expired.TTL = 0
			expired.ResumedFromCursor = nil
			expired.HistoryTruncated = nil
			var t *time.Timer
			t = time.AfterFunc(change.TTL, func() {
				w.mem.Lock()
				if w.mem.expiries[key] != t {
					w.mem.Unlock()
					return
				}
				delete(w.mem.expiries, key)
				w.mem.Unlock()
				TrackMCPEventSubscription(context.WithoutCancel(ctx), server, expired)
			})
			w.mem.expiries[key] = t
		}
	})
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
			w := eventsTarget(server)
			if w == nil {
				return
			}
			opts, whom, base := w.eventsBase(ctx, d.EventsIdentity)
			p := map[string]any{
				"subscription_id": d.SubscriptionID,
				"event_name":      d.EventName,
				"event_id":        d.EventID,
				"delivery_status": deliveryStatus(outcome, err != nil),
			}
			if err == nil && outcome.HTTPStatus != 0 {
				p["http_status"] = outcome.HTTPStatus
			}
			if err == nil && outcome.AttemptCount >= 1 {
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
		})
		return outcome, err
	}
}
