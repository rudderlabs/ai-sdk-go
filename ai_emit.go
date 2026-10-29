package rudderstackai

import (
	"fmt"
	"time"
	"unicode/utf8"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// emitAI sends one AI event, unless WithSampleRates samples it out. Never panics; without a client
// the event is dropped with a warning.
func emitAI(client analytics.Client, event, userID, anonymousID string, properties, msgContext map[string]any, integration string, at time.Time) {
	safely(func() {
		if client == nil {
			warnOnce("no-client", "no RudderStack client configured (Configure(WithAnalytics(client))); AI events are dropped", 0)
			debugf("dropped", "event", event)
			return
		}
		rememberClient(client)
		properties, kept := sample(event, properties, cfg().sampleRates)
		if !kept {
			return
		}
		wrapper := map[string]any{
			"name":        LibraryName,
			"version":     Version,
			"specVersion": SpecVersion,
			"integration": first(integration, IntegrationManual),
		}
		extra := jsonSafeMap(msgContext)
		if extra == nil {
			extra = map[string]any{}
		}
		// A wrapperLibrary set by an outer layer is nested inside ours, never replaced.
		if outer, ok := extra["wrapperLibrary"]; ok {
			wrapper["wrapperLibrary"] = outer
		}
		extra["wrapperLibrary"] = wrapper
		msg := analytics.Track{
			Event:             event,
			UserId:            userID,
			AnonymousId:       anonymousID,
			Properties:        analytics.Properties(fitProperties(event, jsonSafeMap(properties))),
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

// maxPropertiesBytes is the budget for an event's serialized properties: RudderStack rejects an
// event over 32 KiB, and the rest of the message keeps about 2.7 KB. Identical in Node.js and Python.
const maxPropertiesBytes = 30_000

func propertiesBytes(p map[string]any) int {
	b, err := compactJSON(p)
	if err != nil {
		return 0 // not serializable here: left to the client, as before
	}
	return len(b)
}

// fitProperties keeps properties within maxPropertiesBytes, so an oversized event keeps its
// metrics rather than being dropped whole: captured text goes first (response_text, prompt_text,
// action_details.feedback_text), then tool_breakdown and usage_breakdown entries from the end.
func fitProperties(event string, properties map[string]any) map[string]any {
	if propertiesBytes(properties) <= maxPropertiesBytes {
		return properties
	}
	p := make(map[string]any, len(properties))
	for k, v := range properties {
		p[k] = v
	}
	fits := func() bool { return propertiesBytes(p) <= maxPropertiesBytes }
	trimmed := func() map[string]any {
		warnOnce("event-too-large", fmt.Sprintf("%s was over %d bytes; trimmed it", event, maxPropertiesBytes), 0)
		return p
	}
	for _, key := range []string{"response_text", "prompt_text"} {
		if _, ok := p[key]; !ok {
			continue
		}
		delete(p, key)
		if fits() {
			return trimmed()
		}
	}
	if details, ok := p["action_details"].(map[string]any); ok {
		if _, has := details["feedback_text"]; has {
			rest := make(map[string]any, len(details))
			for k, v := range details {
				if k != "feedback_text" {
					rest[k] = v
				}
			}
			p["action_details"] = rest
			if fits() {
				return trimmed()
			}
		}
	}
	for _, key := range []string{"tool_breakdown", "usage_breakdown"} {
		entries, ok := p[key].([]map[string]any)
		if !ok {
			continue
		}
		kept := append([]map[string]any(nil), entries...)
		p[key] = kept
		for len(kept) > 0 && !fits() {
			kept = kept[:len(kept)-1]
			p[key] = kept
		}
		if len(kept) == 0 {
			delete(p, key)
		}
		if fits() {
			return trimmed()
		}
	}
	return trimmed()
}

// fitToolEventProperties keeps a declared tool event within maxPropertiesBytes: when over, the
// rule's Properties output (the custom keys) is dropped and the SDK-owned fields are kept, as when
// Properties panics. fitProperties has nothing of its own to trim on these events.
func fitToolEventProperties(event string, properties map[string]any, custom []string) map[string]any {
	if propertiesBytes(properties) <= maxPropertiesBytes {
		return properties
	}
	p := make(map[string]any, len(properties))
	for k, v := range properties {
		p[k] = v
	}
	for _, k := range custom {
		delete(p, k)
	}
	warnOnce("tool-event-too-large", fmt.Sprintf("%s was over %d bytes; sent without its Properties output", event, maxPropertiesBytes), 0)
	return p
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
	if c.redactHook == nil || masked == redactionTimedOut {
		return masked, true
	}
	if !callback("WithRedactHook", func() (err error) { out, err = c.redactHook(masked); return err }) {
		return "", false
	}
	return out, true
}
