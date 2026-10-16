package rudderstackai

import (
	"time"
	"unicode/utf8"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// emitAI sends one AI event. Never panics; without a client the event is dropped with a warning.
func emitAI(client analytics.Client, event, userID, anonymousID string, properties, msgContext map[string]any, integration string, at time.Time) {
	safely(func() {
		if client == nil {
			warnOnce("no-client", "no RudderStack client configured (Configure(WithAnalytics(client))); AI events are dropped", 0)
			debugf("dropped", "event", event)
			return
		}
		rememberClient(client)
		wrapper := map[string]any{
			"name":        LibraryName,
			"version":     Version,
			"specVersion": SpecVersion,
			"integration": first(integration, IntegrationManual),
		}
		extra := make(map[string]any, len(msgContext)+1)
		for k, v := range msgContext {
			extra[k] = v
		}
		// A wrapperLibrary set by an outer layer is nested inside ours, never replaced.
		if outer, ok := msgContext["wrapperLibrary"]; ok {
			wrapper["wrapperLibrary"] = outer
		}
		extra["wrapperLibrary"] = wrapper
		msg := analytics.Track{
			Event:             event,
			UserId:            userID,
			AnonymousId:       anonymousID,
			Properties:        analytics.Properties(properties),
			OriginalTimestamp: at.UTC(),
			Context:           &analytics.Context{Extra: extra},
		}
		if userID == "" && anonymousID == "" {
			msg.AnonymousId = systemAnonymousID
		}
		if err := client.Enqueue(msg); err != nil {
			warnOnce("track-failed", "RudderStack client Enqueue failed; events may be lost", 0)
			debugf("enqueue failed; event dropped", "event", event, "error", err)
		}
	})
}

// redactText masks one opt-in text field: the built-in patterns, then the hook. ok is false
// (drop the field) when the hook fails or the text is too long to mask.
func redactText(text string) (out string, ok bool) {
	c := cfg()
	if !c.redact {
		return text, true
	}
	if utf8.RuneCountInString(text) > maxMaskedCodePoints {
		debugf("text over the masking size cap; dropping the text field")
		return "", false
	}
	masked := maskBuiltIn(text)
	if c.redactHook == nil {
		return masked, true
	}
	ok = false
	safely(func() {
		var err error
		out, err = c.redactHook(masked)
		ok = err == nil
	})
	if !ok {
		debugf("redact hook failed; dropping the text field")
		return "", false
	}
	return out, true
}
