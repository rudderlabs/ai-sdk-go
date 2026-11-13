package spanmodel

import (
	"strconv"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type object = map[string]any

func dropped(o object, key string, n int) {
	if n > 0 {
		o[key] = n
	}
}

// ToOTLP maps a normalized span through the shared privacy policy.
func ToOTLP(s Span, capture, masked bool) (map[string]any, []map[string]any) {
	sc := s.Context
	status := object{"code": s.StatusCode}
	if capture && s.StatusMessage != "" {
		if message, ok := hook.Tracing.Redact(s.StatusMessage); ok && message != "" {
			status["message"] = message
		}
	}
	encoded := object{
		"traceId": sc.TraceID, "spanId": sc.SpanID, "name": s.Name, "kind": int(s.Kind),
		"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10), "endTimeUnixNano": strconv.FormatInt(s.End.UnixNano(), 10),
		"attributes": keyValues(cleanAttributes(s.Attributes, capture, false, masked), s.AttributeKeys), "status": status,
	}
	if s.Parent.SpanID != "" {
		encoded["parentSpanId"] = s.Parent.SpanID
	}
	if v := sc.TraceState; v != "" {
		encoded["traceState"] = v
	}
	dropped(encoded, "droppedAttributesCount", s.DroppedAttributes)
	dropped(encoded, "droppedEventsCount", s.DroppedEvents)
	dropped(encoded, "droppedLinksCount", s.DroppedLinks)
	events := []object{}
	for _, e := range s.Events {
		if !capture && contentEvent(e.Name) {
			continue
		}
		event := object{"timeUnixNano": strconv.FormatInt(e.Time.UnixNano(), 10), "name": e.Name, "attributes": keyValues(cleanAttributes(e.Attributes, capture, contentEvent(e.Name), masked), e.AttributeKeys)}
		dropped(event, "droppedAttributesCount", e.DroppedAttributeCount)
		events = append(events, event)
	}
	encoded["events"] = events
	links := []object{}
	for _, l := range s.Links {
		link := object{"traceId": l.SpanContext.TraceID, "spanId": l.SpanContext.SpanID, "attributes": keyValues(cleanAttributes(l.Attributes, capture, false, masked), l.AttributeKeys)}
		if v := l.SpanContext.TraceState; v != "" {
			link["traceState"] = v
		}
		dropped(link, "droppedAttributesCount", l.DroppedAttributeCount)
		links = append(links, link)
	}
	encoded["links"] = links
	scope := s.Scope
	scopeJSON := object{"name": scope.Name}
	if scope.Version != "" {
		scopeJSON["version"] = scope.Version
	}
	if len(scope.Attributes) > 0 {
		scopeJSON["attributes"] = keyValues(cleanAttributes(scope.Attributes, capture, false, masked), scope.AttributeKeys)
	}
	ss := object{"scope": scopeJSON, "spans": []object{encoded}}
	if scope.SchemaURL != "" {
		ss["schemaUrl"] = scope.SchemaURL
	}
	resourceAttrs := keyValues(cleanAttributes(s.ResourceAttributes, capture, false, masked), s.ResourceAttributeKeys)
	rs := object{"resource": object{"attributes": resourceAttrs}, "scopeSpans": []object{ss}}
	if s.ResourceSchemaURL != "" {
		rs["schemaUrl"] = s.ResourceSchemaURL
	}
	// Truncation visits attributes in reference order, including resource and links.
	candidates := []object{}
	collectStrings(resourceAttrs, &candidates)
	collectStrings(encoded["attributes"].([]object), &candidates)
	for _, e := range events {
		collectStrings(e["attributes"].([]object), &candidates)
	}
	for _, l := range links {
		collectStrings(l["attributes"].([]object), &candidates)
	}
	return object{"resourceSpans": []object{rs}}, candidates
}

func collectStrings(kvs []object, out *[]object) {
	for _, kv := range kvs {
		collectValue(kv["value"].(object), out)
	}
}

func collectValue(v object, out *[]object) {
	if _, ok := v["stringValue"]; ok {
		*out = append(*out, v)
	}
	if a, ok := v["arrayValue"].(object); ok {
		for _, item := range a["values"].([]object) {
			collectValue(item, out)
		}
	}
	if kvs, ok := v["kvlistValue"].(object); ok {
		collectStrings(kvs["values"].([]object), out)
	}
}
