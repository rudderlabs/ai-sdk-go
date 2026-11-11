package rudderotel

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestProductEventsConformance(t *testing.T) {
	var fixtures struct {
		Cases []struct {
			Name   string
			Config struct {
				Prices map[string]struct {
					Provider                              string
					InputPerMtok, OutputPerMtok           float64
					CachedInputPerMtok, CacheWritePerMtok *float64
				}
				CaptureContent, EmitLLMCalls, ToolBreakdown bool
				Analytics                                   string
				Classifier                                  *struct {
					Intents  []string
					Classify struct{ Intent, Outcome string }
					DelayMS  int `json:"delay_ms"`
				}
			}
			Interaction struct {
				InteractionID       string `json:"interactionId"`
				ConversationID      string `json:"conversationId"`
				UserID              string `json:"userId"`
				Feature, PromptText string
			}
			Steps []struct {
				Op, ResponseText string
				Span             fixtureSpan
				Call             struct {
					Integration, Provider, Model, RequestedModel, FinishReason string
					InputTokens, OutputTokens                                  int
					ResponseID                                                 string   `json:"responseId"`
					Start                                                      string   `json:"startTimeUnixNano"`
					End                                                        string   `json:"endTimeUnixNano"`
					ToolCallIDs                                                []string `json:"toolCallIds"`
				}
			}
			Expected []object
		}
	}
	loadCases(t, "../spec/fixtures/tracing-product-events-v0.1.json", &fixtures)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			r, events := recorder(t)
			prices := map[string]rudderstackai.ModelPrice{}
			for model, p := range c.Config.Prices {
				prices[model] = rudderstackai.ModelPrice{Provider: p.Provider, TokenPrice: rudderstackai.TokenPrice{InputPerMtok: p.InputPerMtok, OutputPerMtok: p.OutputPerMtok, CachedInputPerMtok: p.CachedInputPerMtok, CacheWritePerMtok: p.CacheWritePerMtok}}
			}
			config := []rudderstackai.ConfigOption{rudderstackai.WithPrices(prices), rudderstackai.WithCaptureContent(c.Config.CaptureContent), rudderstackai.WithLLMCallEvents(c.Config.EmitLLMCalls), rudderstackai.WithToolBreakdown(c.Config.ToolBreakdown)}
			if c.Config.Analytics != "processor_only" {
				config = append(config, rudderstackai.WithAnalytics(r))
			}
			if classifier := c.Config.Classifier; classifier != nil {
				config = append(config, rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{Intents: classifier.Intents, Classify: func(ctx context.Context, _ rudderstackai.ClassifyTurn) (rudderstackai.ClassifyResult, error) {
					timer := time.NewTimer(time.Duration(classifier.DelayMS) * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-ctx.Done():
						return rudderstackai.ClassifyResult{}, ctx.Err()
					}
					return rudderstackai.ClassifyResult{Intent: classifier.Classify.Intent, Outcome: classifier.Classify.Outcome}, nil
				}}))
			}
			if err := rudderstackai.Configure(config...); err != nil {
				t.Fatal(err)
			}
			p, err := NewSpanProcessor(Options{Analytics: r, Simple: true, ProductEvents: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: c.Interaction.InteractionID, ConversationID: c.Interaction.ConversationID, UserID: c.Interaction.UserID, Feature: c.Interaction.Feature, PromptText: c.Interaction.PromptText})
			t.Cleanup(func() { i.End(rudderstackai.EndOptions{}) })
			offset := time.Now().UnixNano() - int64(1760000000000000000)
			shift := func(s string) string { n, _ := strconv.ParseInt(s, 10, 64); return strconv.FormatInt(n+offset, 10) }
			for _, step := range c.Steps {
				switch step.Op {
				case "span":
					step.Span.Start, step.Span.End = shift(step.Span.Start), shift(step.Span.End)
					s, _ := startFixture(ctx, t, step.Span, p)
					s.End(trace.WithTimestamp(nanos(step.Span.End)))
				case "wrapper_call":
					c := step.Call
					accepted := i.RecordCall(rudderstackai.LLMCall{Integration: c.Integration, Provider: c.Provider, Model: c.Model, RequestedModel: c.RequestedModel, InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, ResponseID: c.ResponseID, FinishReason: c.FinishReason, StartedAt: nanos(shift(c.Start)), EndedAt: nanos(shift(c.End))})
					if accepted {
						for _, id := range c.ToolCallIDs {
							i.RecordToolCalls(rudderstackai.ToolCall{ID: id})
						}
					}
				case "end":
					i.End(rudderstackai.EndOptions{ResponseText: step.ResponseText})
				case "flush":
					if err := p.ForceFlush(ctx); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown step %q", step.Op)
				}
			}
			if err := p.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			actual := []object{}
			for _, event := range events() {
				if event["event"] != DefaultEventName {
					actual = append(actual, event)
				}
			}
			if len(actual) != len(c.Expected) {
				t.Fatalf("got %d product events want %d: %v", len(actual), len(c.Expected), actual)
			}
			for n, want := range c.Expected {
				matchSubset(t, actual[n], want, "event")
			}
		})
	}
}

func TestSpanOutputTextParts(t *testing.T) {
	for _, c := range []struct{ name, input, want string }{
		{"mixed content", `[null,{"parts":{}},{"parts":[null,{"type":"image","content":{"url":"image"}},{"type":"text","content":"hello "},{"type":"text","content":"world"}]}]`, "hello world"},
		{"empty text wins over legacy", `[{"parts":[{"type":"text","content":""}]}]`, ""},
		{"invalid JSON uses legacy", `invalid`, "legacy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(capture))
			_, span := provider.Tracer("test").Start(context.Background(), "chat", trace.WithAttributes(
				attribute.String("gen_ai.output.messages", c.input), attribute.String("gen_ai.completion.0.content", "legacy"),
			))
			span.End()
			if got := spanCall(capture.Ended()[0]).OutputText; got != c.want {
				t.Fatalf("output=%q want %q", got, c.want)
			}
			if err := provider.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
