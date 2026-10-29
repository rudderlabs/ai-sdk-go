package rudderstackai

import (
	"context"
	"maps"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// TrackActionOptions configures [TrackAction].
type TrackActionOptions struct {
	// ActionType is one of the standard action types.
	ActionType ActionType
	// InteractionID of the answer the user acted on. Required.
	InteractionID string
	// ConversationID of the answer. Required: pass it from your client with the interaction ID
	// (falls back to the context).
	ConversationID string
	// ActionDetails such as {"feedback_type": "thumbs", "feedback_value": 1}, or card_kind and
	// target_type for clicked_through. feedback_text is free text, dropped unless content
	// capture is on.
	ActionDetails map[string]any
	UserID        string
	AnonymousID   string
	AccountID     string
	OnBehalfOf    string
	// AgentVersion names the app release or configuration; Variant names its experiment arm.
	AgentVersion string
	Variant      string
	Surface      Surface
	// Analytics is the client for this event. Falls back to the context, then the global client.
	Analytics analytics.Client
	// Context is extra message context for this event.
	Context map[string]any
}

// TrackAction sends ai_user_action server-side, typically from a feedback endpoint. An event that
// cannot be made conformant (no InteractionID or ConversationID) is dropped with a warning.
// Sends nothing with WithServerFeedback(false). Never sets source: a server copy has none.
func TrackAction(ctx context.Context, o TrackActionOptions) {
	safely(func() {
		c := cfg()
		if !c.serverFeedback {
			debugf("ai_user_action not sent: server feedback is off")
			return
		}
		cv := valuesFrom(ctx)
		if cv == nil {
			cv = &contextValues{}
		}
		conversation := first(o.ConversationID, cv.conversationID)
		if o.InteractionID == "" || conversation == "" {
			warnf("ai_user_action dropped: InteractionID and ConversationID are required")
			return
		}
		if !actionTypes[o.ActionType] {
			warnf("non-standard action_type %s", o.ActionType)
		}
		p := map[string]any{"conversation_id": conversation, "interaction_id": o.InteractionID, "action_type": string(o.ActionType)}
		put(p, "account_id", first(o.AccountID, cv.accountID))
		put(p, "on_behalf_of", first(o.OnBehalfOf, cv.onBehalfOf))
		put(p, "agent_version", first(o.AgentVersion, cv.agentVersion))
		put(p, "variant", first(o.Variant, cv.variant))
		put(p, "surface", string(first2(surfaceOf(o.Surface), surfaceOf(cv.surface))))
		if o.ActionDetails != nil {
			details := maps.Clone(o.ActionDetails)
			capture := c.captureContent
			if i := FromContext(ctx); i != nil {
				capture = i.captureContent
			}
			if !capture {
				delete(details, "feedback_text")
			}
			// The spec types these as strings: drop a wrong-typed value rather than send an
			// invalid event.
			for _, k := range []string{"feedback_type", "feedback_text", "card_kind", "target_type"} {
				v, has := details[k]
				if !has {
					continue
				}
				s, ok := v.(string)
				if !ok || (s == "" && (k == "card_kind" || k == "target_type")) {
					warnf("action_details.%s must be a string; dropped", k)
					delete(details, k)
				}
			}
			if text, ok := details["feedback_text"].(string); ok {
				if masked, ok := redactText(text); ok {
					details["feedback_text"] = masked
				} else {
					delete(details, "feedback_text")
				}
			}
			if len(details) > 0 {
				p["action_details"] = details
			}
		}
		sendStandalone(cv, "ai_user_action", o.UserID, o.AnonymousID, o.Analytics, o.Context, p)
	})
}
