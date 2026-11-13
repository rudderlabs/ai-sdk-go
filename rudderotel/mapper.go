package rudderotel

import (
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
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
	extra := options.ExtraProperties
	if options.ExtraPropertiesFrom != nil {
		safe("tracing.extraProperties", func() { extra = options.ExtraPropertiesFrom(s) })
	}
	id := Identity{}
	if options.ResolveIdentity != nil {
		safe("tracing.resolveIdentity", func() { id = options.ResolveIdentity(s) })
	}
	return spanmodel.ToTrack(normalizedSpan(s), spanmodel.Options{CaptureContent: options.CaptureContent, MaxEventBytes: options.MaxEventBytes, EventName: options.EventName, ExtraProperties: extra, UserID: id.UserID, AnonymousID: id.AnonymousID, LibraryName: rudderstackai.LibraryName, LibraryVersion: rudderstackai.Version})
}
