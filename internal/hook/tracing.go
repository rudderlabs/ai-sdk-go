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
	SetClock                 func(any, func() time.Time)
	RecordSpan               func(any, any, *SpanSource)
	RecordAncestry           func(any, string, string, string)
	EmitSpans                func() bool
	ParentContext            func(context.Context) (traceID, spanID, traceState string)
	WrapperCaptureContent    func(any) bool
	WrapperMetadata          func(any, string, string)
	FinishRaw                func(any, string, string, int, time.Time, error)
	ActivateWrapper          func(context.Context, any) context.Context
	BeginWrapper             func(context.Context, any, time.Time) (context.Context, any)
	ReserveWrapperCompletion func(any) func()
	RecordWrapper            func(any, any, any, int) bool
	Identity                 func(context.Context, string) (attributes map[string]string, system, suppressed bool, owner any)
	Open                     func(string) any
	// Latest is the newest segment of an interaction's run in this process (itself unless restored).
	Latest         func(any) any
	AdoptAnalytics func(any, analytics.Client)
	RecordTool     func(any, any, bool)
	Analytics      func() analytics.Client
	CaptureLevel   func() *capture.Policy
	Redact         func(string) (string, bool)
	StripBinary    func(any) any
	NormalizeValue func(any) (any, bool)
	RememberClient func(analytics.Client)
	Wait           func(context.Context) bool
	FlushClient    func(context.Context, analytics.Client) bool
	// ProductOwner returns the live ambient interaction when its ID matches.
	// Otherwise, it returns an owner only if exactly one live run matches the ID: an open or
	// paused interaction, or a paused original whose snapshot was restored in this process
	// (counted as its restored run), and that run has not ended.
	ProductOwner func(string, any) any
	// Seq is the registry's current sequence, for LateOwner.
	Seq func() uint64
	// LateOwner is ProductOwner without an ambient interaction for an ID a span acquired after
	// it started at sequence since: nil also when a run with the ID ended or was collected since.
	LateOwner func(id string, since uint64) any
	// NextSpanSeq orders SDK spans and processor-seen OTel model-call spans by start.
	NextSpanSeq func() uint64
	// SDKOwnsCall is the active SDK call span in ctx that a model-call span starting now belongs
	// to, if any.
	SDKOwnsCall func(ctx context.Context, interaction any) (SDKCall, bool)
	// AdoptOTelCall matches the active, still-recording OTel model-call span in ctx that a span
	// processor registered, when it belongs to interaction and is nested inside the active SDK
	// span with start order activeSeq (0 if none). The first call adopts its IDs and marks it
	// replaced, so every processor drops it at its end; later calls inside are children. fits is
	// asked, before anything is marked and with no lock held (it can run app code), whether the
	// SDK span can be sent under the IDs it would take; when it cannot, or the span's state
	// changed while it ran, nothing is adopted and the OTel span exports normally.
	// Set by rudderotel.
	AdoptOTelCall func(ctx context.Context, interaction any, activeSeq uint64, fits func(OTelCall) bool) (OTelCall, bool)
}

// SDKCall is an SDK call span an OTel model-call span starts inside. Cover marks that the OTel
// span is not exported, so the SDK span sheds optional fields instead of being dropped; false,
// marking nothing, when the call already ended.
type SDKCall struct {
	TraceID, SpanID string
	Cover           func() bool
}

// OTelCall is an OTel model-call span an SDK call starts inside.
type OTelCall struct {
	TraceID, SpanID, ParentID string
	// Adopted: this call is the first, and takes the span's trace, span and parent IDs; otherwise
	// the call is a child of the span with SpanID.
	Adopted bool
}
