package rudderotel

import (
	"math"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type object = map[string]any

func anyValue(v attribute.Value) object {
	switch v.Type() {
	case attribute.STRING:
		return object{"stringValue": v.AsString()}
	case attribute.BOOL:
		return object{"boolValue": v.AsBool()}
	case attribute.INT64:
		return object{"intValue": strconv.FormatInt(v.AsInt64(), 10)}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		var value any = f
		switch {
		case math.IsNaN(f):
			value = "NaN"
		case math.IsInf(f, 1):
			value = "Infinity"
		case math.IsInf(f, -1):
			value = "-Infinity"
		}
		return object{"doubleValue": value}
	case attribute.SLICE:
		values := make([]object, 0, len(v.AsSlice()))
		for _, x := range v.AsSlice() {
			values = append(values, anyValue(x))
		}
		return object{"arrayValue": object{"values": values}}
	case attribute.MAP:
		return object{"kvlistValue": object{"values": keyValues(v.AsMap())}}
	case attribute.STRINGSLICE, attribute.BOOLSLICE, attribute.INT64SLICE, attribute.FLOAT64SLICE:
		values := []object{}
		switch v.Type() {
		case attribute.STRINGSLICE:
			for _, x := range v.AsStringSlice() {
				values = append(values, anyValue(attribute.StringValue(x)))
			}
		case attribute.BOOLSLICE:
			for _, x := range v.AsBoolSlice() {
				values = append(values, anyValue(attribute.BoolValue(x)))
			}
		case attribute.INT64SLICE:
			for _, x := range v.AsInt64Slice() {
				values = append(values, anyValue(attribute.Int64Value(x)))
			}
		case attribute.FLOAT64SLICE:
			for _, x := range v.AsFloat64Slice() {
				values = append(values, anyValue(attribute.Float64Value(x)))
			}
		}
		return object{"arrayValue": object{"values": values}}
	}
	return object{}
}

func keyValues(attrs []attribute.KeyValue) []object {
	out := make([]object, 0, len(attrs))
	for _, kv := range attrs {
		out = append(out, object{"key": string(kv.Key), "value": anyValue(kv.Value)})
	}
	return out
}

func dropped(o object, key string, n int) {
	if n > 0 {
		o[key] = n
	}
}

func otlpStatus(code codes.Code) int {
	switch code {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	default:
		return 0
	}
}

func spanToOTLP(s sdktrace.ReadOnlySpan, capture bool) (object, []object) {
	sc := s.SpanContext()
	status := object{"code": otlpStatus(s.Status().Code)}
	if capture && s.Status().Description != "" {
		if message, ok := hook.Tracing.Redact(s.Status().Description); ok && message != "" {
			status["message"] = message
		}
	}
	encoded := object{
		"traceId": sc.TraceID().String(), "spanId": sc.SpanID().String(), "name": s.Name(), "kind": int(s.SpanKind()),
		"startTimeUnixNano": strconv.FormatInt(s.StartTime().UnixNano(), 10), "endTimeUnixNano": strconv.FormatInt(s.EndTime().UnixNano(), 10),
		"attributes": keyValues(cleanAttributes(s.Attributes(), capture, false)), "status": status,
	}
	if s.Parent().HasSpanID() {
		encoded["parentSpanId"] = s.Parent().SpanID().String()
	}
	if v := sc.TraceState().String(); v != "" {
		encoded["traceState"] = v
	}
	dropped(encoded, "droppedAttributesCount", s.DroppedAttributes())
	dropped(encoded, "droppedEventsCount", s.DroppedEvents())
	dropped(encoded, "droppedLinksCount", s.DroppedLinks())
	events := []object{}
	for _, e := range s.Events() {
		if !capture && contentEvent(e.Name) {
			continue
		}
		event := object{"timeUnixNano": strconv.FormatInt(e.Time.UnixNano(), 10), "name": e.Name, "attributes": keyValues(cleanAttributes(e.Attributes, capture, contentEvent(e.Name)))}
		dropped(event, "droppedAttributesCount", e.DroppedAttributeCount)
		events = append(events, event)
	}
	encoded["events"] = events
	links := []object{}
	for _, l := range s.Links() {
		link := object{"traceId": l.SpanContext.TraceID().String(), "spanId": l.SpanContext.SpanID().String(), "attributes": keyValues(cleanAttributes(l.Attributes, capture, false))}
		if v := l.SpanContext.TraceState().String(); v != "" {
			link["traceState"] = v
		}
		dropped(link, "droppedAttributesCount", l.DroppedAttributeCount)
		links = append(links, link)
	}
	encoded["links"] = links
	scope := s.InstrumentationScope()
	scopeJSON := object{"name": scope.Name}
	if scope.Version != "" {
		scopeJSON["version"] = scope.Version
	}
	if scope.Attributes.Len() > 0 {
		scopeJSON["attributes"] = keyValues(cleanAttributes(scope.Attributes.ToSlice(), capture, false))
	}
	ss := object{"scope": scopeJSON, "spans": []object{encoded}}
	if scope.SchemaURL != "" {
		ss["schemaUrl"] = scope.SchemaURL
	}
	resourceAttrs := keyValues(cleanAttributes(s.Resource().Attributes(), capture, false))
	rs := object{"resource": object{"attributes": resourceAttrs}, "scopeSpans": []object{ss}}
	if s.Resource().SchemaURL() != "" {
		rs["schemaUrl"] = s.Resource().SchemaURL()
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
