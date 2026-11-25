package rudderstackai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// emitAI sends one AI event, unless WithSampleRates samples it out. Never panics; without a client
// the event is dropped with a warning.
func emitAI(client analytics.Client, event, userID, anonymousID string, properties, msgContext map[string]any, integration string, at time.Time) {
	emitAIRates(client, event, userID, anonymousID, properties, msgContext, integration, at, cfg().sampleRates)
}

// emitAIRates is emitAI under one rates snapshot, so an event fitted under it is sampled under it.
func emitAIRates(client analytics.Client, event, userID, anonymousID string, properties, msgContext map[string]any, integration string, at time.Time, rates map[string]float64) {
	safely(func() {
		if client == nil {
			warnOnce("no-client", "no RudderStack client configured (Configure(WithAnalytics(client))); AI events are dropped", 0)
			debugf("dropped", "event", event)
			return
		}
		rememberClient(client)
		properties, kept := sample(event, properties, rates)
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
		if !fitMessage(event, &msg) {
			return
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
// metrics rather than being dropped whole: a call's input_messages shrink first (oldest after the
// system prompt, then the longest strings), then captured text goes (output_text,
// output_tool_calls, response_text, prompt_text, action_details.feedback_text), then
// tool_breakdown and usage_breakdown entries from the end.
func fitProperties(event string, properties map[string]any) map[string]any {
	return fitPropertiesWithin(event, properties, maxPropertiesBytes)
}

// fitPropertiesWithin is fitProperties with a byte budget of its own.
func fitPropertiesWithin(event string, properties map[string]any, budget int) map[string]any {
	if propertiesBytes(properties) <= budget {
		return properties
	}
	p := make(map[string]any, len(properties))
	for k, v := range properties {
		p[k] = v
	}
	fits := func() bool { return propertiesBytes(p) <= budget }
	trimmed := func() map[string]any {
		warnOnce("event-too-large", fmt.Sprintf("%s was over %d bytes; trimmed it", event, budget), 0)
		return p
	}
	if input, ok := p["input_messages"].([]any); ok {
		// Output that overflows on its own goes first, so the input is not shortened for nothing.
		rest := make(map[string]any, len(p))
		for k, v := range p {
			if k != "input_messages" {
				rest[k] = v
			}
		}
		for _, key := range []string{"output_text", "output_tool_calls"} {
			if propertiesBytes(rest) <= budget {
				break
			}
			delete(rest, key)
			delete(p, key)
		}
		p["input_messages"] = input
		if fits() {
			return trimmed()
		}
		// Oldest messages after the system prompt go first, then the longest strings are shortened.
		p["input_truncated"] = true
		if fitted, ok := trimInputMessages(input, propertiesBytes(p)-budget); ok {
			p["input_messages"] = fitted
		} else {
			delete(p, "input_messages")
			delete(p, "input_truncated")
		}
		if fits() {
			return trimmed()
		}
	}
	for _, key := range []string{"output_text", "output_tool_calls", "response_text", "prompt_text"} {
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
		// Other details (a large feedback_value) go last; the IDs, action_type and sample_rate stay.
		delete(p, "action_details")
		if fits() {
			return trimmed()
		}
	}
	// Sampling metadata shares the budget with application response extras; drop them in bulk
	// so the response is serialized once, not per extra.
	if event == "ai_response_received" {
		dropped := false
		for key := range p {
			if !reservedProperties[key] && !responseMetricProperties[key] {
				delete(p, key)
				dropped = true
			}
		}
		if dropped && fits() {
			return trimmed()
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

// maxMessageBytes is the RudderStack client's default MaxMessageBytes: it drops a larger message
// and logs it with its payload.
const maxMessageBytes = 32_768

// fitMessage keeps msg within maxMessageBytes, so the client never logs it: properties shrink into
// the room the rest leaves, then the app's context goes (our wrapperLibrary stays), else the event
// is dropped (false).
func fitMessage(event string, msg *analytics.Track) bool {
	if n, ok := boundBytes(*msg); ok && n <= maxMessageBytes {
		return true // the common case: no encoding, no allocation
	}
	fits := func() bool { return clientBytes(*msg) <= maxMessageBytes }
	refit := func() {
		rest := *msg
		rest.Properties = analytics.Properties{}
		room := maxMessageBytes - (clientBytes(rest) - len("{}"))
		msg.Properties = analytics.Properties(fitPropertiesWithin(event, msg.Properties, room))
	}
	if fits() {
		return true
	}
	refit()
	if fits() {
		return true
	}
	msg.Context = &analytics.Context{Extra: map[string]any{"wrapperLibrary": msg.Context.Extra["wrapperLibrary"]}}
	refit()
	if fits() {
		return true
	}
	warnOnce("event-dropped-too-large", event+" was over 32 KiB after trimming; dropped it", 0)
	return false
}

// clientBytes is msg's size as the client measures it: json.Marshal (HTML escaped) of msg with the
// fields Enqueue adds before its size check. Counted, not buffered.
func clientBytes(msg analytics.Track) int {
	msg.Type = "track"
	msg.MessageId = first(msg.MessageId, idPlaceholder)
	msg.AnonymousId = first(msg.AnonymousId, idPlaceholder)
	msg.SentAt = msg.OriginalTimestamp
	msg.Channel = "server"
	c := analytics.Context{}
	if msg.Context != nil {
		c = *msg.Context
	}
	c.Library = analytics.LibraryInfo{Name: "analytics-go", Version: analytics.Version}
	msg.Context = &c
	var n byteCount
	if err := json.NewEncoder(&n).Encode(msg); err != nil {
		return 0 // left to the client, as before
	}
	return int(n) - 1 // Encode's newline
}

// boundBytes is at least msg's clientBytes, without encoding: the exact escaped size of its
// properties, context and IDs, plus the rest of the message measured once. ok is false when a value
// is not one jsonSafe produces.
func boundBytes(msg analytics.Track) (int, bool) {
	props, ok := marshalBytes(map[string]any(msg.Properties))
	if !ok || msg.Context == nil {
		return 0, false
	}
	extra, ok := marshalBytes(msg.Context.Extra)
	if !ok {
		return 0, false
	}
	return envelopeBytes + props + extra + quotedBytes(msg.Event) + quotedBytes(msg.UserId) + quotedBytes(msg.AnonymousId), true
}

// envelopeBytes bounds a message's size without its properties, context extras, event and IDs:
// the field names, the timestamps at their longest, the library and the generated IDs.
var envelopeBytes = clientBytes(analytics.Track{
	Properties:        analytics.Properties{},
	OriginalTimestamp: time.Date(2006, 1, 2, 15, 4, 5, 123456789, time.UTC),
	Context:           &analytics.Context{Extra: map[string]any{}},
}) + 64 // field names omitted when empty, and separators

// marshalBytes is v's exact size under json.Marshal for the values jsonSafe leaves (numbers bounded).
func marshalBytes(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return len("null"), true
	case bool:
		return len("false"), true
	case string:
		return quotedBytes(x), true
	case json.Number:
		return len(x) + 1, true
	case json.RawMessage:
		return len(x) + 5*escapes(string(x)), true // compact already; Marshal escapes HTML
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return 25, true // the longest a number encodes to
	case []string:
		n := 2 + max(len(x)-1, 0)
		for _, e := range x {
			n += quotedBytes(e)
		}
		return n, true
	case []any:
		n := 2 + max(len(x)-1, 0)
		for _, e := range x {
			b, ok := marshalBytes(e)
			if !ok {
				return 0, false
			}
			n += b
		}
		return n, true
	case []map[string]any:
		n := 2 + max(len(x)-1, 0)
		for _, e := range x {
			b, _ := marshalBytes(e)
			n += b
		}
		return n, true
	case map[string]any:
		n := 2 + max(len(x)-1, 0)
		for k, e := range x {
			b, ok := marshalBytes(e)
			if !ok {
				return 0, false
			}
			n += quotedBytes(k) + 1 + b
		}
		return n, true
	}
	return 0, false
}

// quotedBytes is the size of s as a JSON string under json.Marshal (HTML escaped).
func quotedBytes(s string) int {
	n := 2
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
				n += 2
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || r == '\u2028' || r == '\u2029' {
			n += 6
		} else {
			n += size
		}
		i += size
	}
	return n
}

// escapes counts what json.Marshal escapes in compact JSON (<, >, &, U+2028, U+2029); each adds
// at most 5 bytes.
func escapes(s string) int {
	return strings.Count(s, "<") + strings.Count(s, ">") + strings.Count(s, "&") + strings.Count(s, "\u2028") + strings.Count(s, "\u2029")
}

// idPlaceholder stands in for the UUIDs Enqueue generates.
const idPlaceholder = "00000000-0000-0000-0000-000000000000"

type byteCount int

func (n *byteCount) Write(p []byte) (int, error) {
	*n += byteCount(len(p))
	return len(p), nil
}

// fitToolEventProperties keeps a declared tool event within maxPropertiesBytes: when over, the
// rule's Properties output (the custom keys) is dropped and the SDK-owned fields are kept, as when
// Properties panics. fitProperties has nothing of its own to trim on these events.
func fitToolEventProperties(event string, properties map[string]any, custom []string, rates map[string]float64) map[string]any {
	properties = sampleProperties(event, properties, rates)
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

var responseMetricProperties = set("sample_rate", "block_reason", "agent_version", "variant", "confidence", "compaction_count", "total_credits", "pause_count", "total_paused_ms")
