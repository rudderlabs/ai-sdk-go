package rudderotel

import (
	"encoding/json"
	"log/slog"
	"time"
	"unicode/utf8"

	analytics "github.com/rudderlabs/analytics-go/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

const (
	DefaultEventName       = "ai_otel_span"
	SchemaVersion          = "0.1"
	DefaultMaxEventBytes   = 32_000
	AnonymousIDAttribute   = "rudderstack.anonymous_id"
	InteractionIDAttribute = "rudderstack.interaction_id"
	truncationSuffix       = "...[truncated]"
)

// Identity is the RudderStack identity of an exported span.
type Identity struct{ UserID, AnonymousID string }

// Options configures a span exporter, processor, or the setup helper.
// Pointer booleans distinguish an explicit false from inherited defaults.
type Options struct {
	Analytics           analytics.Client
	CaptureContent      *bool
	ExportAllSpans      bool
	Filter              func(sdktrace.ReadOnlySpan) bool
	ResolveIdentity     func(sdktrace.ReadOnlySpan) Identity
	MaxEventBytes       int
	EventName           string
	ExtraProperties     map[string]any
	ExtraPropertiesFrom func(sdktrace.ReadOnlySpan) map[string]any
	// Simple exports synchronously as spans end; otherwise OTel batches them.
	Simple            bool
	BatchOptions      []sdktrace.BatchSpanProcessorOption
	PropagateIdentity *bool
	ProductEvents     bool
	LinkTraceID       bool
}

func iso(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// SpanToTrack maps one ended span; nil means it could not fit the event budget.
// It does not apply the export filter. The original span is never mutated.
func SpanToTrack(s sdktrace.ReadOnlySpan, options Options) (*analytics.Track, error) {
	capture := hook.Tracing.CaptureContent()
	if options.CaptureContent != nil {
		capture = *options.CaptureContent
	}
	otlp, candidates := spanToOTLP(s, capture)
	sc := s.SpanContext()
	var parent any
	if s.Parent().HasSpanID() {
		parent = s.Parent().SpanID().String()
	}
	duration := max(s.EndTime().Sub(s.StartTime()).Microseconds(), 0)
	status := "unset"
	switch otlpStatus(s.Status().Code) {
	case 1:
		status = "ok"
	case 2:
		status = "error"
	}
	kind := s.SpanKind().String()
	p := object{
		"schema_version": SchemaVersion, "trace_id": sc.TraceID().String(), "span_id": sc.SpanID().String(), "parent_span_id": parent,
		"span_name": s.Name(), "span_kind": kind, "start_time": iso(s.StartTime()), "end_time": iso(s.EndTime()),
		"duration_ms": float64(duration) / 1000, "status_code": status, "truncated": false, "otlp": otlp,
	}
	if s.Status().Description != "" {
		p["status_message"] = s.Status().Description
	}
	a := attributes(s.Attributes())
	if v := spanOrResource(s, "session.id"); v != "" {
		p["session_id"] = v
	}
	if v := firstString(a, InteractionIDAttribute); v != "" {
		p["interaction_id"] = v
	}
	if v := firstString(a, "gen_ai.conversation.id"); v != "" {
		p["conversation_id"] = v
	}
	if g := genAISummary(a); len(g) > 0 {
		p["gen_ai"] = g
	}
	extra := options.ExtraProperties
	if options.ExtraPropertiesFrom != nil {
		safe("tracing.extraProperties", func() { extra = options.ExtraPropertiesFrom(s) })
	}
	for k, v := range extra {
		if _, exists := p[k]; !exists {
			p[k] = v
		}
	}
	id := Identity{
		UserID:      spanOrResource(s, "user.id", "traceloop.association.properties.user_id"),
		AnonymousID: spanOrResource(s, AnonymousIDAttribute, "session.id", "traceloop.association.properties.session_id"),
	}
	if options.ResolveIdentity != nil {
		safe("tracing.resolveIdentity", func() {
			if custom := options.ResolveIdentity(s); custom.UserID != "" || custom.AnonymousID != "" {
				id = custom
			}
		})
	}
	if id.AnonymousID == "" {
		id.AnonymousID = sc.TraceID().String()
	}
	event := options.EventName
	if event == "" {
		event = DefaultEventName
	}
	m := &analytics.Track{Event: event, UserId: id.UserID, AnonymousId: id.AnonymousID,
		MessageId: sc.TraceID().String() + "-" + sc.SpanID().String(), OriginalTimestamp: s.EndTime().UTC().Truncate(time.Millisecond),
		Properties: analytics.Properties(p), Context: &analytics.Context{Extra: object{"wrapperLibrary": object{
			"name": rudderstackai.LibraryName, "version": rudderstackai.Version, "integration": "tracing",
		}}},
	}
	maxBytes := options.MaxEventBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxEventBytes
	}
	measure := func() (int, error) { b, err := json.Marshal(m); return len(b), err }
	size, err := measure()
	if err != nil {
		return nil, err
	}
	for range 1000 {
		if size <= maxBytes {
			return m, nil
		}
		longest, n := -1, 0
		for i, c := range candidates {
			if size := len(c["stringValue"].(string)); size > n {
				longest, n = i, size
			}
		}
		if longest < 0 || n <= 256 {
			break
		}
		target := max(256, n-(size-maxBytes)-len(truncationSuffix))
		value := candidates[longest]["stringValue"].(string)
		end := min(target, len(value))
		for end > 0 && end < len(value) && !utf8.RuneStart(value[end]) {
			end--
		}
		shortened := value[:end] + truncationSuffix
		if len(shortened) >= n {
			candidates = append(candidates[:longest], candidates[longest+1:]...)
			continue
		}
		candidates[longest]["stringValue"] = shortened
		p["truncated"] = true
		size, err = measure()
		if err != nil {
			return nil, err
		}
	}
	if size <= maxBytes {
		return m, nil
	}
	slog.Warn("[rudderstack-ai] span exceeds maxEventBytes after truncation; dropped", "span_id", sc.SpanID().String(), "maxEventBytes", maxBytes)
	return nil, nil
}
