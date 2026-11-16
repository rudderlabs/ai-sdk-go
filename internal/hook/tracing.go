package hook

import (
	"context"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
)

// Tracing is the core's integration API for span inputs. It contains no OTel types.
// Interaction is opaque here to avoid an import cycle; integrations use *Interaction.
var Tracing struct {
	// SetClock supplies a per-interaction clock for portable lifecycle conformance tests.
	SetClock              func(any, func() time.Time)
	RecordSpan            func(any, any, *SpanSource)
	RecordAncestry        func(any, string, string, string)
	EmitSpans             func() bool
	ParentContext         func(context.Context) (traceID, spanID, traceState string)
	WrapperCaptureContent func(any) bool
	WrapperMetadata       func(any, string, string)
	FinishRaw             func(any, string, string, int, time.Time, error)
	ActivateWrapper       func(context.Context, any) context.Context
	BeginWrapper          func(context.Context, any, time.Time) (context.Context, any)
	RecordWrapper         func(any, any, any, int) bool
	Identity              func(context.Context, string) (attributes map[string]string, system, suppressed bool, owner any)
	Open                  func(string) any
	AdoptAnalytics        func(any, analytics.Client)
	RecordTool            func(any, any)
	Analytics             func() analytics.Client
	CaptureLevel          func() *capture.Policy
	Redact                func(string) (string, bool)
	StripBinary           func(any) any
	NormalizeValue        func(any) (any, bool)
	RememberClient        func(analytics.Client)
	Wait                  func(context.Context) bool
	FlushClient           func(context.Context, analytics.Client) bool
	// NextSpanSeq orders SDK spans and processor-seen OTel model-call spans by start.
	NextSpanSeq func() uint64
	// SDKOwnsCall reports whether the active SDK span in ctx is a model call of interaction.
	SDKOwnsCall func(ctx context.Context, interaction any) bool
	// OTelCallOwner returns the active, still-recording OTel model-call span in ctx that a span
	// processor registered: its IDs, start order and interaction. Set by rudderotel.
	OTelCallOwner func(ctx context.Context) (traceID, spanID string, seq uint64, interaction any, ok bool)
}
