package spanmodel

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"sort"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// CompletionReserveBytes is the room an admitted core keeps for completion: duration, status
// and a parent ID set later.
const CompletionReserveBytes = 96

var (
	mandatoryAttributes = map[string]bool{
		"gen_ai.operation.name": true, "gen_ai.provider.name": true, "gen_ai.system": true,
		"gen_ai.request.model": true, "gen_ai.conversation.id": true, "rudderstack.interaction_id": true,
		"rudderstack.span_source": true, "user.id": true, "rudderstack.anonymous_id": true,
	}
	mandatorySummary = map[string]bool{"operation_name": true, "provider_name": true, "request_model": true, "conversation_id": true}
)

// otlpSpan is the one resource span, scope span and span of a message's OTLP payload.
func otlpSpan(p object) (rs, ss, span object) {
	rs = p["otlp"].(object)["resourceSpans"].([]object)[0]
	ss = rs["scopeSpans"].([]object)[0]
	span = ss["spans"].([]object)[0]
	return rs, ss, span
}

// DeliveryCore is the mandatory core of an SDK span's message (FIXTURE-FORMAT.md, SDK span
// delivery budget), at the shortest truncation. ok when it plus CompletionReserveBytes fits the
// budget: only then may the span replace or cover an OTel span, and shedding never removes it.
func DeliveryCore(s Span, options Options) (core *analytics.Track, ok bool) {
	m, _, _, maxBytes := build(s, options)
	core = cloneTrack(m)
	p := object(core.Properties)
	delete(p, "status_message")
	if summary, isMap := p["gen_ai"].(object); isMap {
		for k := range summary {
			if !mandatorySummary[k] {
				delete(summary, k)
			}
		}
	}
	rs, ss, span := otlpSpan(p)
	span["attributes"] = slices.DeleteFunc(span["attributes"].([]object), func(kv object) bool { return !mandatoryAttributes[kv["key"].(string)] })
	span["events"], span["links"] = []object{}, []object{}
	delete(span, "droppedAttributesCount")
	delete(span, "droppedEventsCount")
	delete(span, "droppedLinksCount")
	delete(span["status"].(object), "message")
	rs["resource"].(object)["attributes"] = []object{}
	delete(ss["scope"].(object), "attributes")
	for _, c := range otlpStrings(rs, ss, span) {
		value := c["stringValue"].(string)
		if shortened := cutValue(value, minTruncatedValueBytes); len(shortened) < len(value) {
			c["stringValue"] = shortened
		}
	}
	p["truncated"] = true
	return core, jsonSize(core)+CompletionReserveBytes <= maxBytes
}

// otlpStrings are the string values of an OTLP span, in the order truncation visits them.
func otlpStrings(rs, ss, span object) []object {
	out := []object{}
	collectStrings(rs["resource"].(object)["attributes"].([]object), &out)
	collectStrings(span["attributes"].([]object), &out)
	for _, e := range span["events"].([]object) {
		collectStrings(e["attributes"].([]object), &out)
	}
	for _, l := range span["links"].([]object) {
		collectStrings(l["attributes"].([]object), &out)
	}
	_ = ss
	return out
}

// shed makes an SDK span that replaced or covered an OTel span fit the budget without dropping
// it: content candidates first, then other optional ones, each group largest first, until the
// message fits. If it still does not, the admitted core is restored with the current IDs,
// timing, status, message ID and timestamp. The order is the shared contract in
// FIXTURE-FORMAT.md, "SDK span delivery budget".
func shed(m, admitted *analytics.Track, maxBytes int) {
	core := cloneTrack(admitted)
	p := object(m.Properties)
	rs, ss, span := otlpSpan(p)
	cp := object(core.Properties)
	_, _, coreSpan := otlpSpan(cp)
	admittedKeys := map[string]bool{}
	for _, kv := range coreSpan["attributes"].([]object) {
		admittedKeys[kv["key"].(string)] = true
	}
	summary, _ := p["gen_ai"].(object)
	if summary == nil {
		summary = object{}
		p["gen_ai"] = summary
	}
	coreSummary, _ := cp["gen_ai"].(object)
	p["truncated"] = true
	scopeName, _ := ss["scope"].(object)["name"].(string)

	type candidate struct {
		content bool
		bytes   int
		remove  func()
	}
	var candidates []candidate
	add := func(value any, content bool, remove func()) {
		candidates = append(candidates, candidate{content, jsonSize(value), remove})
	}
	// entries removes one entry of an OTLP list by identity, keeping the others' order.
	entries := func(owner object, field string, content func(object) bool, skip func(object) bool) {
		list, _ := owner[field].([]object)
		ordered := slices.Clone(list)
		if field == "attributes" {
			sort.SliceStable(ordered, func(i, j int) bool {
				return bytes.Compare([]byte(ordered[i]["key"].(string)), []byte(ordered[j]["key"].(string))) < 0
			})
		}
		for _, e := range ordered {
			if skip != nil && skip(e) {
				continue
			}
			add(e, content(e), func() {
				current := owner[field].([]object)
				owner[field] = slices.DeleteFunc(slices.Clone(current), func(x object) bool { return sameEntry(x, e) })
			})
		}
	}
	isContent := func(kv object) bool { return contentAttribute(kv["key"].(string), scopeName) }
	entries(span, "attributes", isContent, func(kv object) bool { return admittedKeys[kv["key"].(string)] })
	entries(rs["resource"].(object), "attributes", isContent, nil)
	if _, has := ss["scope"].(object)["attributes"]; has {
		entries(ss["scope"].(object), "attributes", isContent, nil)
	}
	entries(span, "events", func(e object) bool { return contentEvent(e["name"].(string)) }, nil)
	entries(span, "links", func(object) bool { return false }, nil)
	status := span["status"].(object)
	if message, has := status["message"]; has {
		add(message, true, func() { delete(status, "message") })
	}
	if message, has := p["status_message"]; has {
		add(message, true, func() { delete(p, "status_message") })
	}
	for _, k := range sortedKeys(summary) {
		if _, kept := coreSummary[k]; !kept {
			add(summary[k], false, func() { delete(summary, k) })
		}
	}
	for _, k := range sortedKeys(p) {
		if _, kept := cp[k]; !kept {
			add(p[k], false, func() { delete(p, k) })
		}
	}
	for _, k := range []string{"droppedAttributesCount", "droppedEventsCount", "droppedLinksCount"} {
		if v, has := span[k]; has {
			add(v, false, func() { delete(span, k) })
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].content != candidates[j].content {
			return candidates[i].content
		}
		return candidates[i].bytes > candidates[j].bytes
	})
	for _, c := range candidates {
		if jsonSize(m) <= maxBytes {
			break
		}
		c.remove()
	}
	if jsonSize(m) <= maxBytes {
		return
	}
	// Completion can enlarge core values too: keep their admitted values, with the current IDs,
	// timing, status, message ID and timestamp.
	for _, k := range []string{"trace_id", "span_id", "parent_span_id", "start_time", "end_time", "duration_ms", "status_code"} {
		cp[k] = p[k]
	}
	for _, k := range []string{"traceId", "spanId", "startTimeUnixNano", "endTimeUnixNano"} {
		coreSpan[k] = span[k]
	}
	if parent, has := span["parentSpanId"]; has {
		coreSpan["parentSpanId"] = parent
	} else {
		delete(coreSpan, "parentSpanId")
	}
	coreSpan["status"] = object{"code": status["code"]}
	core.MessageId, core.OriginalTimestamp, core.SentAt = m.MessageId, m.OriginalTimestamp, m.SentAt
	*m = *core
}

// sameEntry reports whether two OTLP list entries are the same map.
func sameEntry(a, b object) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

func sortedKeys(o object) []string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare([]byte(keys[i]), []byte(keys[j])) < 0 })
	return keys
}

func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// cloneTrack deep-copies a track message's properties and context, keeping the map and slice
// types the mapper builds.
func cloneTrack(m *analytics.Track) *analytics.Track {
	c := *m
	c.Properties = analytics.Properties(cloneValue(object(m.Properties)).(object))
	if m.Context != nil {
		ctx := *m.Context
		if m.Context.Extra != nil {
			ctx.Extra = cloneValue(object(m.Context.Extra)).(object)
		}
		c.Context = &ctx
	}
	return &c
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case object:
		out := make(object, len(x))
		for k, item := range x {
			out[k] = cloneValue(item)
		}
		return out
	case analytics.Properties:
		return cloneValue(object(x))
	case []object:
		out := make([]object, len(x))
		for i, item := range x {
			out[i] = cloneValue(item).(object)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}
