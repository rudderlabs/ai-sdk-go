package rudderotel

import (
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

type object = map[string]any

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
	return spanmodel.ToOTLP(normalizedSpan(s), capture, false)
}
