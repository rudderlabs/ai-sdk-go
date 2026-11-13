package rudderotel

import (
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

func normalizedAttributes(a []attribute.KeyValue) map[string]any {
	out := map[string]any{}
	for _, kv := range a {
		out[string(kv.Key)] = normalizedValue(kv.Value)
	}
	return out
}
func normalizedValue(v attribute.Value) any {
	switch v.Type() {
	case attribute.MAP:
		return normalizedAttributes(v.AsMap())
	case attribute.SLICE:
		values := v.AsSlice()
		out := make([]any, len(values))
		for i, x := range values {
			out[i] = normalizedValue(x)
		}
		return out
	case attribute.BYTESLICE:
		return v.AsByteSlice()
	default:
		return v.AsInterface()
	}
}
func attributeKeys(a []attribute.KeyValue) []string {
	out := make([]string, len(a))
	for i, kv := range a {
		out[i] = string(kv.Key)
	}
	return out
}
func normalizedSpan(s sdktrace.ReadOnlySpan) spanmodel.Span {
	sc, parent, scope := s.SpanContext(), s.Parent(), s.InstrumentationScope()
	n := spanmodel.Span{Name: s.Name(), Kind: int(s.SpanKind()), Context: spanmodel.Context{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), TraceState: sc.TraceState().String()}, Start: s.StartTime(), End: s.EndTime(), StatusCode: otlpStatus(s.Status().Code), StatusMessage: s.Status().Description, Attributes: normalizedAttributes(s.Attributes()), AttributeKeys: attributeKeys(s.Attributes()), ResourceAttributeKeys: attributeKeys(s.Resource().Attributes()), ResourceAttributes: normalizedAttributes(s.Resource().Attributes()), ResourceSchemaURL: s.Resource().SchemaURL(), Scope: spanmodel.Scope{Name: scope.Name, Version: scope.Version, SchemaURL: scope.SchemaURL, Attributes: normalizedAttributes(scope.Attributes.ToSlice()), AttributeKeys: attributeKeys(scope.Attributes.ToSlice())}, DroppedAttributes: s.DroppedAttributes(), DroppedEvents: s.DroppedEvents(), DroppedLinks: s.DroppedLinks()}
	if parent.HasSpanID() {
		n.Parent = spanmodel.Context{TraceID: parent.TraceID().String(), SpanID: parent.SpanID().String(), TraceState: parent.TraceState().String()}
	}
	for _, e := range s.Events() {
		n.Events = append(n.Events, spanmodel.Event{Name: e.Name, Time: e.Time, Attributes: normalizedAttributes(e.Attributes), AttributeKeys: attributeKeys(e.Attributes), DroppedAttributeCount: e.DroppedAttributeCount})
	}
	for _, l := range s.Links() {
		n.Links = append(n.Links, spanmodel.Link{SpanContext: spanmodel.Context{TraceID: l.SpanContext.TraceID().String(), SpanID: l.SpanContext.SpanID().String(), TraceState: l.SpanContext.TraceState().String()}, Attributes: normalizedAttributes(l.Attributes), AttributeKeys: attributeKeys(l.Attributes), DroppedAttributeCount: l.DroppedAttributeCount})
	}
	return n
}
