package hook

import (
	"context"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// Tracing is the core's integration API for span inputs. It contains no OTel types.
// Interaction is opaque here to avoid an import cycle; integrations use *Interaction.
var Tracing struct {
	Identity       func(context.Context, string) (attributes map[string]string, system, suppressed bool, owner any)
	Open           func(string) any
	AdoptAnalytics func(any, analytics.Client)
	RecordTool     func(any, any)
	Analytics      func() analytics.Client
	CaptureContent func() bool
	Redact         func(string) (string, bool)
	RememberClient func(analytics.Client)
	Wait           func(context.Context) bool
	FlushClient    func(context.Context, analytics.Client) bool
}
