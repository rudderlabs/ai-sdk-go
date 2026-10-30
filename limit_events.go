package rudderstackai

import (
	"context"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// TrackLimitReachedOptions describes an app-enforced usage limit. Only LimitType is required.
type TrackLimitReachedOptions struct {
	LimitType, Feature                                 string
	UserID, AnonymousID, ConversationID, InteractionID string
	AccountID, OnBehalfOf, AgentVersion, Variant       string
	Surface                                            Surface
	Analytics                                          analytics.Client
	Context                                            map[string]any
}

// TrackLimitReached emits ai_limit_reached. Provider rate limits remain model call errors.
func TrackLimitReached(ctx context.Context, o TrackLimitReachedOptions) {
	safely(func() {
		if o.LimitType == "" {
			warnf("ai_limit_reached needs a non-empty LimitType; dropped")
			return
		}
		cv := valuesFrom(ctx)
		if cv == nil {
			cv = &contextValues{}
		}
		p := map[string]any{"limit_type": o.LimitType}
		put(p, "feature", first(o.Feature, cv.feature))
		put(p, "conversation_id", first(o.ConversationID, cv.conversationID))
		put(p, "interaction_id", o.InteractionID)
		put(p, "account_id", first(o.AccountID, cv.accountID))
		put(p, "on_behalf_of", first(o.OnBehalfOf, cv.onBehalfOf))
		put(p, "agent_version", first(o.AgentVersion, cv.agentVersion))
		put(p, "variant", first(o.Variant, cv.variant))
		put(p, "surface", string(first2(surfaceOf(o.Surface), surfaceOf(cv.surface))))
		sendStandalone(cv, "ai_limit_reached", o.UserID, o.AnonymousID, o.Analytics, o.Context, p)
	})
}
