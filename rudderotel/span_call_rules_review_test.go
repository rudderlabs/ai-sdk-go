package rudderotel

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func reviewAnswer(t *testing.T, events []object) object {
	t.Helper()
	for _, event := range events {
		if event["event"] == "ai_response_received" {
			return event["properties"].(object)
		}
	}
	t.Fatal("missing answer")
	return nil
}

func TestSpanRulesFailedParentInvariants(t *testing.T) {
	for _, childError := range []bool{false, true} {
		for _, same := range []bool{false, true} {
			for _, overlaps := range []bool{false, true} {
				t.Run(fmt.Sprintf("childError=%t/sameModel=%t/overlaps=%t", childError, same, overlaps), func(t *testing.T) {
					provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
					ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
					start := time.Now()
					tracer := provider.Tracer("test")
					parentCtx, parent := tracer.Start(ctx, "failed parent", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "model-A")))
					model, childStart, childEnd := "model-B", 901*time.Millisecond, time.Second
					if same {
						model = "model-A"
					}
					if overlaps {
						childStart, childEnd = 200*time.Millisecond, 800*time.Millisecond
					}
					_, child := tracer.Start(parentCtx, "child", trace.WithTimestamp(start.Add(childStart)), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", model), attribute.String("gen_ai.response.id", "child-response")))
					if childError {
						child.SetStatus(codes.Error, "failed")
					}
					child.End(trace.WithTimestamp(start.Add(childEnd)))
					parent.SetStatus(codes.Error, "failed")
					parent.End(trace.WithTimestamp(start.Add(900 * time.Millisecond)))
					i.End(rudderstackai.EndOptions{})
					want := float64(2)
					if childError && same && overlaps {
						want = 1
					}
					if got := reviewAnswer(t, events())["llm_call_count"]; got != want {
						t.Fatalf("calls=%v want %v", got, want)
					}
				})
			}
		}
	}
}

func TestSpanRulesSkippedUsageCannotSuppressPrep(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	start := time.Now()
	tracer := provider.Tracer("test")
	ctx, root := tracer.Start(ctx, "root")
	defer root.End()
	parentCtx, parent := tracer.Start(ctx, "usage parent", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "model-A"), attribute.Int("gen_ai.usage.input_tokens", 100)))
	_, child := tracer.Start(parentCtx, "retained child", trace.WithTimestamp(start.Add(20*time.Millisecond)), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "model-B"), attribute.String("gen_ai.response.id", "retained-child")))
	child.End(trace.WithTimestamp(start.Add(800 * time.Millisecond)))
	parent.End(trace.WithTimestamp(start.Add(900 * time.Millisecond)))
	_, prep := tracer.Start(ctx, "unrelated prep", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("openinference.span.kind", "LLM"), attribute.String("llm.model_name", "model-prep")))
	prep.End(trace.WithTimestamp(start.Add(10 * time.Millisecond)))
	i.End(rudderstackai.EndOptions{})
	answer := reviewAnswer(t, events())
	if answer["llm_call_count"] != float64(2) {
		t.Fatal(answer)
	}
	breakdown := answer["usage_breakdown"].([]any)
	if len(breakdown) != 2 || breakdown[0].(object)["model"] != "model-B" || breakdown[1].(object)["model"] != "model-prep" {
		t.Fatal(breakdown)
	}
}

func TestSpanRulesSuppressionCyclesAllEndOrders(t *testing.T) {
	var visit func([]int, []int)
	visit = func(order, remaining []int) {
		if len(remaining) > 0 {
			for j, n := range remaining {
				rest := append([]int(nil), remaining[:j]...)
				rest = append(rest, remaining[j+1:]...)
				visit(append(append([]int(nil), order...), n), rest)
			}
			return
		}
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			start := time.Now()
			spans := []trace.Span{}
			ctx, root := provider.Tracer("test").Start(ctx, "root")
			defer root.End()
			for _, model := range []string{"group-A", "group-B"} {
				parentCtx, parent := provider.Tracer("test").Start(ctx, "usage parent", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("openinference.span.kind", "LLM"), attribute.String("llm.model_name", model), attribute.Int("llm.token_count.prompt", 100)))
				_, prep := provider.Tracer("test").Start(parentCtx, "prep child", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("openinference.span.kind", "LLM"), attribute.String("llm.model_name", model)))
				spans = append(spans, parent, prep)
			}
			for _, n := range order {
				duration := 900 * time.Millisecond
				if n%2 == 1 {
					duration = 10 * time.Millisecond
				}
				spans[n].End(trace.WithTimestamp(start.Add(duration)))
			}
			i.End(rudderstackai.EndOptions{})
			breakdown, ok := reviewAnswer(t, events())["usage_breakdown"].([]any)
			if !ok {
				t.Fatal("no calls retained")
			}
			for _, model := range []string{"group-A", "group-B"} {
				found := false
				for _, usage := range breakdown {
					if usage.(object)["model"] == model && usage.(object)["call_count"].(float64) >= 1 {
						found = true
					}
				}
				if !found {
					t.Fatalf("no call retained for %s: %v", model, breakdown)
				}
			}
		})
	}
	visit(nil, []int{0, 1, 2, 3})
}

func TestSpanRulesOpenNonModelDoesNotDefer(t *testing.T) {
	for _, operation := range []string{"execute_tool", "invoke_agent", "chain"} {
		t.Run(operation, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			start := time.Now()
			tracer := provider.Tracer("test")
			parentCtx, callA := tracer.Start(ctx, "A", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "model-A"), attribute.Int("gen_ai.usage.input_tokens", 100)))
			_, descendant := tracer.Start(parentCtx, "non-model", trace.WithTimestamp(start.Add(100*time.Millisecond)), trace.WithAttributes(attribute.String("gen_ai.operation.name", operation), attribute.String("gen_ai.tool.name", "lookup")))
			callA.End(trace.WithTimestamp(start.Add(200 * time.Millisecond)))
			count := func(want int) {
				t.Helper()
				if got := int(reflect.ValueOf(i).Elem().FieldByName("callCount").Int()); got != want {
					t.Fatalf("recorded calls=%d want %d", got, want)
				}
			}
			count(1)
			_, callB := tracer.Start(ctx, "B", trace.WithTimestamp(start.Add(300*time.Millisecond)), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "model-B"), attribute.Int("gen_ai.usage.input_tokens", 200)))
			callB.End(trace.WithTimestamp(start.Add(400 * time.Millisecond)))
			count(2)
			descendant.End(trace.WithTimestamp(start.Add(800 * time.Millisecond)))
			i.End(rudderstackai.EndOptions{})
			// The final answer retains A before B.
			var answer object
			for _, event := range events() {
				if event["event"] == "ai_response_received" {
					answer = event["properties"].(object)
				}
			}
			breakdown := answer["usage_breakdown"].([]any)
			if breakdown[0].(object)["model"] != "model-A" || breakdown[1].(object)["model"] != "model-B" {
				t.Fatal(breakdown)
			}
		})
	}
}

func TestSpanRulesRicherParentSurvivesEmptyDescendant(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(parentFirst), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			start := time.Now()
			ctx, parent := provider.Tracer("test").Start(ctx, "parent", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("openinference.span.kind", "LLM"), attribute.String("gen_ai.request.model", "model"), attribute.Int("gen_ai.usage.input_tokens", 100)))
			_, child := provider.Tracer("test").Start(ctx, "empty child", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("openinference.span.kind", "LLM"), attribute.String("gen_ai.request.model", "model")))
			if parentFirst {
				parent.End(trace.WithTimestamp(start.Add(time.Millisecond)))
				child.End(trace.WithTimestamp(start.Add(time.Millisecond)))
			} else {
				child.End(trace.WithTimestamp(start.Add(time.Millisecond)))
				parent.End(trace.WithTimestamp(start.Add(time.Millisecond)))
			}
			i.End(rudderstackai.EndOptions{})
			answer := reviewAnswer(t, events())
			if answer["llm_call_count"] != float64(2) || answer["total_input_tokens"] != float64(100) {
				t.Fatal(answer)
			}
		})
	}
}
