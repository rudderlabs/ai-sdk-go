package spanmodel

import (
	"encoding/json"
	"log/slog"
	"maps"
	"time"
	"unicode/utf8"

	analytics "github.com/rudderlabs/analytics-go/v4"

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

// Options controls the normalized mapper without an OTel runtime dependency.
type Options struct {
	CaptureContent *bool
	ContentMasked  bool
	// SDKIdentity uses the identity captured by the SDK, without OTel fallbacks.
	SDKIdentity                 bool
	MessageContext              map[string]any
	MaxEventBytes               int
	EventName                   string
	ExtraProperties             map[string]any
	UserID, AnonymousID         string
	LibraryName, LibraryVersion string
}

func iso(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// SpanToTrack maps one ended span; nil means it could not fit the event budget.
// It does not apply the export filter. The original span is never mutated.
func ToTrack(s Span, options Options) (*analytics.Track, error) {
	capture := hook.Tracing.CaptureContent()
	if options.CaptureContent != nil {
		capture = *options.CaptureContent
	}
	s.Attributes = cleanAttributes(s.Attributes, capture, false, options.ContentMasked)
	otlp, candidates := toOTLP(s, capture, options.ContentMasked, s.Attributes)
	sc := s.Context
	var parent any
	if s.Parent.SpanID != "" {
		parent = s.Parent.SpanID
	}
	duration := max(s.End.Sub(s.Start).Microseconds(), 0)
	status := "unset"
	switch s.StatusCode {
	case 1:
		status = "ok"
	case 2:
		status = "error"
	}
	kind := kindName(s.Kind)
	p := object{
		"schema_version": SchemaVersion, "trace_id": sc.TraceID, "span_id": sc.SpanID, "parent_span_id": parent,
		"span_name": s.Name, "span_kind": kind, "start_time": iso(s.Start), "end_time": iso(s.End),
		"duration_ms": float64(duration) / 1000, "status_code": status, "truncated": false, "otlp": otlp,
	}
	resourceSpan := otlp["resourceSpans"].([]object)[0]
	scopeSpan := resourceSpan["scopeSpans"].([]object)[0]
	span := scopeSpan["spans"].([]object)[0]
	if message, ok := span["status"].(object)["message"]; ok {
		p["status_message"] = message
	}
	a := s.Attributes
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
	for k, v := range extra {
		if _, exists := p[k]; !exists {
			p[k] = v
		}
	}
	id := struct{ UserID, AnonymousID string }{
		UserID:      spanOrResource(s, "user.id", "traceloop.association.properties.user_id"),
		AnonymousID: spanOrResource(s, AnonymousIDAttribute, "session.id", "traceloop.association.properties.session_id"),
	}
	if options.SDKIdentity || options.UserID != "" || options.AnonymousID != "" {
		id.UserID, id.AnonymousID = options.UserID, options.AnonymousID
	}
	if !options.SDKIdentity && id.AnonymousID == "" {
		id.AnonymousID = sc.TraceID
	}
	event := options.EventName
	if event == "" {
		event = DefaultEventName
	}
	extraContext := maps.Clone(options.MessageContext)
	if extraContext == nil {
		extraContext = object{}
	}
	wrapper := object{"name": options.LibraryName, "version": options.LibraryVersion, "integration": "tracing"}
	if outer, ok := extraContext["wrapperLibrary"]; ok {
		wrapper["wrapperLibrary"] = outer
	}
	extraContext["wrapperLibrary"] = wrapper
	m := &analytics.Track{Event: event, UserId: id.UserID, AnonymousId: id.AnonymousID,
		MessageId: sc.TraceID + "-" + sc.SpanID, OriginalTimestamp: s.End.UTC().Truncate(time.Millisecond),
		Properties: analytics.Properties(p), Context: &analytics.Context{Extra: extraContext},
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
	slog.Warn("[rudderstack-ai] span exceeds maxEventBytes after truncation; dropped", "span_id", sc.SpanID, "maxEventBytes", maxBytes)
	return nil, nil
}
