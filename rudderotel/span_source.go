package rudderotel

import (
	"strings"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

func hasModelUsage(s sdktrace.ReadOnlySpan) bool {
	for _, a := range s.Attributes() {
		name := string(a.Key)
		if strings.HasPrefix(name, "gen_ai.usage.") || strings.HasPrefix(name, "llm.token_count.") || name == "mirascope.response.usage" || strings.HasPrefix(name, "ai.usage.") {
			return true
		}
	}
	return false
}

func hasModelOutput(s sdktrace.ReadOnlySpan, call rudderstackai.LLMCall) bool {
	if call.OutputText != "" {
		return true
	}
	for _, a := range s.Attributes() {
		name := string(a.Key)
		if spanmodel.OutputAttribute(name) {
			return true
		}
	}
	return false
}

func spanSource(s sdktrace.ReadOnlySpan, call rudderstackai.LLMCall) *hook.SpanSource {
	a := attributes(s.Attributes())
	parent := ""
	if s.Parent().IsValid() {
		parent = s.Parent().SpanID().String()
	}
	return &hook.SpanSource{TraceID: s.SpanContext().TraceID().String(), SpanID: s.SpanContext().SpanID().String(), ParentSpanID: parent,
		ResponseID: call.ResponseID, RequestModel: call.RequestedModel, ResponseModel: call.Model,
		Start: float64(s.StartTime().Unix())*1000 + float64(s.StartTime().Nanosecond())/1e6, End: float64(s.EndTime().Unix())*1000 + float64(s.EndTime().Nanosecond())/1e6,
		Usage: hasModelUsage(s), Output: hasModelOutput(s, call), Error: s.Status().Code == codes.Error,
		LLM: strings.ToUpper(firstString(a, "openinference.span.kind")) == "LLM", Ancestors: []string{}}
}
