package rudderotel

import (
	"math"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSpanUsageFallbackPrecedence(t *testing.T) {
	usage := attribute.String("mirascope.response.usage", `{"input_tokens":100,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"total_tokens":120,"reasoning_tokens":10}`)
	for _, c := range []struct {
		name  string
		attrs []attribute.KeyValue
		want  [4]int
	}{
		{"openinference wins", []attribute.KeyValue{
			attribute.Int("llm.token_count.prompt", 0), attribute.Int("llm.token_count.completion", 2),
			attribute.Int("llm.token_count.prompt_details.cache_read", 3), attribute.Int("llm.token_count.prompt_details.cache_write", 4),
		}, [4]int{0, 2, 3, 4}},
		{"invalid counts fall through independently", []attribute.KeyValue{
			attribute.Int("gen_ai.usage.input_tokens", -1), attribute.Float64("gen_ai.usage.output_tokens", math.NaN()),
			attribute.String("gen_ai.usage.cache_read.input_tokens", "3"), attribute.Float64("gen_ai.usage.cache_write.input_tokens", 1.5),
			attribute.Int("gen_ai.usage.cache_creation.input_tokens", 5),
		}, [4]int{100, 20, 30, 5}},
		{"cache creation zero wins over later aliases", []attribute.KeyValue{
			attribute.Int("gen_ai.usage.cache_creation.input_tokens", 0), attribute.Int("gen_ai.usage.cache_creation_input_tokens", 6),
			attribute.Int("llm.token_count.prompt_details.cache_write", 7),
		}, [4]int{100, 20, 30, 0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			call := spanCall(tracetest.SpanStub{Attributes: append(c.attrs, usage)}.Snapshot())
			got := [4]int{call.InputTokens, call.OutputTokens, call.CachedInputTokens, call.CacheWriteTokens}
			if got != c.want || call.UsageUnknown {
				t.Fatalf("usage=%v unknown=%v want %v", got, call.UsageUnknown, c.want)
			}
		})
	}
	for _, value := range []attribute.Value{
		attribute.Int64Value(100), attribute.MapValue(attribute.Int("input_tokens", 100)),
		attribute.StringValue(`{"total_tokens":120,"reasoning_tokens":10}`),
		attribute.StringValue(`{"input_tokens":true,"output_tokens":1e100,"cache_read_tokens":null,"cache_write_tokens":-1}`),
	} {
		call := spanCall(tracetest.SpanStub{Attributes: []attribute.KeyValue{{Key: "mirascope.response.usage", Value: value}}}.Snapshot())
		if !call.UsageUnknown || call.InputTokens != 0 || call.OutputTokens != 0 || call.CachedInputTokens != 0 || call.CacheWriteTokens != 0 {
			t.Fatalf("invalid/derived usage accepted: %+v", call)
		}
	}
	call := spanCall(tracetest.SpanStub{Attributes: []attribute.KeyValue{
		attribute.String("mirascope.response.usage", `{"input_tokens":100,"output_tokens":1e400,"cache_read_tokens":30}`),
	}}.Snapshot())
	if call.InputTokens != 100 || call.OutputTokens != 0 || call.CachedInputTokens != 30 || call.UsageUnknown {
		t.Fatalf("overflowing count invalidated other usage: %+v", call)
	}
}

func TestSpanFinishReasonPresenceAndProviderCase(t *testing.T) {
	for _, c := range []struct {
		name   string
		plural []attribute.KeyValue
		want   string
	}{
		{"absent plural", nil, "stop"},
		{"empty plural", []attribute.KeyValue{attribute.StringSlice("gen_ai.response.finish_reasons", nil)}, "legacy"},
		{"invalid plural", []attribute.KeyValue{attribute.Int("gen_ai.response.finish_reasons", 1)}, "legacy"},
		{"plural spelling preserved", []attribute.KeyValue{attribute.StringSlice("gen_ai.response.finish_reasons", []string{"FinishReason.LENGTH"})}, "FinishReason.LENGTH"},
	} {
		t.Run(c.name, func(t *testing.T) {
			attrs := append(c.plural, attribute.String("gen_ai.response.finish_reason", "Some.FinishReason.STOP"),
				attribute.String("llm.finish_reason", "legacy"), attribute.String("llm.provider", "OpenAI.Responses"))
			call := spanCall(tracetest.SpanStub{Attributes: attrs}.Snapshot())
			if call.FinishReason != c.want || call.Provider != "openai" {
				t.Fatalf("reason=%q provider=%q", call.FinishReason, call.Provider)
			}
		})
	}
}

func TestTraceloopClassificationAndContentPrecedence(t *testing.T) {
	for _, c := range []struct {
		attrs []attribute.KeyValue
		want  string
	}{
		{nil, "tool"},
		{[]attribute.KeyValue{attribute.String("gen_ai.operation.name", "invoke_agent")}, ""},
		{[]attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat")}, "llm"},
		{[]attribute.KeyValue{attribute.String("openinference.span.kind", "AGENT")}, ""},
		{[]attribute.KeyValue{attribute.String("openinference.span.kind", "LLM")}, "llm"},
		{[]attribute.KeyValue{attribute.String("llm.request.type", "chat")}, "tool"},
	} {
		s := tracetest.SpanStub{Attributes: append(c.attrs, attribute.String("traceloop.span.kind", "tool"))}.Snapshot()
		if got := spanRole(s); got != c.want || !IsAISpan(s) {
			t.Fatalf("role=%q want %q, filter=%v", got, c.want, IsAISpan(s))
		}
	}
	if IsAISpan(tracetest.SpanStub{Attributes: []attribute.KeyValue{attribute.String("traceloop.span.kind", "workflow")}}.Snapshot()) {
		t.Fatal("non-tool Traceloop span passed the default filter")
	}
	for _, keys := range [][]string{
		{"gen_ai.tool.call.arguments", "input.value", "traceloop.entity.input"},
		{"gen_ai.tool.call.result", "output.value", "traceloop.entity.output"},
	} {
		a := map[string]attribute.Value{}
		for i := len(keys) - 1; i >= 0; i-- {
			a[keys[i]] = attribute.StringValue(`{"source":"` + keys[i] + `"}`)
			want := map[string]any{"source": keys[i]}
			if got := toolValue(a, keys...); !reflect.DeepEqual(got, want) {
				t.Fatalf("tool content=%v want %v", got, want)
			}
		}
	}
}
