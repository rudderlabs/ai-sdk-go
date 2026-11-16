package rudderstackai

import (
	"context"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
)

// ContentCapture is the content capture policy per kind of text, set with [WithContentCapture],
// [InteractionOptions].ContentCapture, [TaskOptions].ContentCapture, the MCP Options and
// rudderotel.Options. A nil field is not set at that level: it falls back to Default, then to the
// level above (global, then interaction, MCP wrapper or tracing exporter); nothing set means off.
// The bool CaptureContent option at the same level is shorthand for Default, and keys set here
// win over it. Masking and length caps always apply.
//
//   - AnswerText: prompt_text and response_text.
//   - LLMCalls: input_messages, output_text and output_tool_calls on ai_llm_response_received.
//   - Spans: span content per kind ([SpanCapture]).
//   - FeedbackText: feedback_text and capability_detail.
//   - ScoreExplanation: the score explanation.
//   - IntentSummary: intent_summary. Only this key gates it (Default and the bool do not).
//   - CallReason: MCP call_reason.
//   - ErrorMessage: MCP error_message.
type ContentCapture = capture.Policy

// SpanCapture is span content per kind: Messages (prompt/completion text, system instructions,
// retrieval, error text, content events, the status message), ToolCalls (tool-call arguments and
// results) and HTTP (url.path, and full URLs without the query string; off unless set itself).
// Default is Messages and ToolCalls when they are not set.
type SpanCapture = capture.SpanPolicy

// WithContentCapture sets the global content capture policy per kind of text. Keys set here win
// over [WithCaptureContent]; see [ContentCapture].
func WithContentCapture(p ContentCapture) ConfigOption {
	return func(c *config, _ *pending) error {
		c.contentCapture = &p
		return nil
	}
}

// captureLevel is the global level.
func (c config) captureLevel() *capture.Policy {
	on := c.captureContent
	return capture.Level(&on, c.contentCapture)
}

// currentCapture is the policy for code running now: ctx's interaction's, else the global one.
func currentCapture(ctx context.Context) capture.Resolved {
	if i := FromContext(ctx); i != nil {
		return i.capture
	}
	return capture.Resolve(cfg().captureLevel())
}
