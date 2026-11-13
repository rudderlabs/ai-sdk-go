package kit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestLateStreamUsesSpanCapturePolicy(t *testing.T) {
	// Restore TestMain's call-event default after the subtests reset core state.
	t.Cleanup(func() {
		if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true)); err != nil {
			t.Error(err)
		}
	})
	for _, capture := range []bool{false, true} {
		t.Run(testCaptureName(capture), func(t *testing.T) {
			r := reviewRecorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(capture)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
			spec := Spec{Provider: "openai", Integration: "late", IsStream: func(*http.Request, map[string]any) bool { return true }, NewStream: func(c *Call, _ *http.Response) StreamHandler {
				o := NewObserver(c)
				o.NoteText("late answer")
				o.NoteTool("tool", "id", "late_tool", map[string]any{"value": "late argument"}, "")
				return &chatStream{Observer: o}
			}}
			resp, err := spec.Intercept(reviewRequest(ctx), func(*http.Request) (*http.Response, error) {
				owner.End(rudderstackai.EndOptions{})
				if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(!capture)); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			spans := r.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d", len(spans))
			}
			raw, _ := json.Marshal(spans[0])
			for _, value := range []string{"late answer", "late_tool", "late argument"} {
				if strings.Contains(string(raw), value) != capture {
					t.Errorf("capture=%v missing or leaked %q: %s", capture, value, raw)
				}
			}
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Error("late product call retained")
			}
		})
	}
}
func testCaptureName(c bool) string {
	if c {
		return "on"
	}
	return "off"
}
