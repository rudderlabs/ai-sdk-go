package rudderstackai

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestManualSpanStripsBlobPayloads(t *testing.T) {
	for _, kind := range []string{"blob", "image-data", "file-data", "uri"} {
		t.Run(kind, func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
			part := map[string]any{"type": kind, "content": "c2VjcmV0", "data": "c2VjcmV0", "bytes": "c2VjcmV0", "base64": "c2VjcmV0", "b64_json": "c2VjcmV0", "mime_type": "application/octet-stream"}
			if kind == "uri" {
				part["uri"] = "DATA:application/octet-stream;base64,c2VjcmV0"
			}
			_, s := StartSpan(context.Background(), SpanOptions{Attributes: map[string]any{"gen_ai.input.messages": []any{map[string]any{"role": "user", "parts": []any{part, map[string]any{"type": "text", "content": "keep"}}}}}})
			s.End(nil)
			got := spanAttributes(r.named("ai_otel_span")[0].Properties)["gen_ai.input.messages"].([]any)[0].(map[string]any)["parts"].([]any)
			want := map[string]any{"type": kind, "mime_type": "application/octet-stream"}
			if kind == "uri" {
				want["type"] = "blob"
			}
			if !reflect.DeepEqual(got, []any{want, map[string]any{"type": "text", "content": "keep"}}) {
				t.Fatalf("payload retained: %v", got)
			}
			if part["content"] != "c2VjcmV0" {
				t.Fatal("caller input mutated")
			}
		})
	}
}

func TestEverySpanCompletionRespectsFlushDeadline(t *testing.T) {
	for _, path := range []string{"normal", "late", "raw", "manual"} {
		for _, shutdown := range []bool{false, true} {
			t.Run(path+"/shutdown="+testBoolName(shutdown), func(t *testing.T) {
				c := &round7BlockingClient{entered: make(chan struct{}), release: make(chan struct{})}
				setupAI(t, WithEmitSpans(true), WithAnalytics(c))
				ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
				var span *Span
				if path == "manual" {
					_, span = StartSpan(ctx, SpanOptions{Name: "manual"})
				} else {
					_, span = beginWrapperSpan(ctx, owner, time.Now())
				}
				if path == "late" {
					owner.End(EndOptions{})
				}
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					switch path {
					case "manual":
						span.End(nil)
					case "raw":
						span.finish("chat m", "chat", "wrapper", nil, nil, true, time.Time{})
					default:
						owner.recordWrapperCall(LLMCall{Provider: "openai", Model: "m"}, span, 200)
					}
				}()
				<-c.entered
				deadline, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				var ok bool
				if shutdown {
					ok = Shutdown(deadline)
				} else {
					ok = Flush(deadline)
				}
				close(c.release)
				<-finished
				if ok {
					t.Error("blocked completion reported success")
				}
				owner.End(EndOptions{})
				if !Flush(context.Background()) {
					t.Error("released completion did not settle")
				}
			})
		}
	}
}
