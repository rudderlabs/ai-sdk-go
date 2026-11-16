// Package capture resolves the content capture policy (SDK-6888) shared by the root package,
// ruddermcpgo, rudderotel and the span mapper.
package capture

// Policy is one level's content capture policy. A nil field is not set at this level and falls
// back to Default, then to the level above; nothing set at any level means off.
type Policy struct {
	// Default is every feature not set at this level. Not IntentSummary, not Spans.HTTP.
	Default *bool
	// AnswerText: prompt_text and response_text.
	AnswerText *bool
	// LLMCalls: input_messages, output_text and output_tool_calls on ai_llm_response_received.
	LLMCalls *bool
	// Spans: span content by kind.
	Spans *SpanPolicy
	// FeedbackText: feedback_text and capability_detail.
	FeedbackText *bool
	// ScoreExplanation: the score explanation.
	ScoreExplanation *bool
	// IntentSummary: intent_summary. Only this key gates it (Default and the bool form do not):
	// unset keeps it sent whenever the classifier or TrackClassification provides one.
	IntentSummary *bool
	// CallReason: MCP call_reason.
	CallReason *bool
	// ErrorMessage: MCP error_message.
	ErrorMessage *bool
}

// SpanPolicy is span content by kind. Content is masked when kept.
type SpanPolicy struct {
	// Default is the message and tool-call kinds not set here. Not HTTP.
	Default *bool
	// Messages: prompt/completion text, system instructions, retrieval, error text, content
	// events and the status message.
	Messages *bool
	// ToolCalls: tool-call arguments and results.
	ToolCalls *bool
	// HTTP: url.path, and url.full / http.url / http.target without the query string. Off unless
	// set itself.
	HTTP *bool
}

// Spans are the span content kinds kept, as resolved.
type Spans struct {
	Messages, ToolCalls, HTTP bool
}

// All is every span content kind kept or removed, as the bool option did (never HTTP).
func All(on bool) Spans { return Spans{Messages: on, ToolCalls: on} }

// Content reports whether any message or tool-call content is kept.
func (s Spans) Content() bool { return s.Messages || s.ToolCalls }

// Resolved is the policy in force for one interaction, MCP request or exported span.
type Resolved struct {
	AnswerText, LLMCalls, FeedbackText, ScoreExplanation, CallReason, ErrorMessage bool
	// IntentSummary is nil when no level sets it: sent as before the policy.
	IntentSummary *bool
	// Fallback is the Default chain alone, for custom properties named intent_summary.
	Fallback bool
	Spans    Spans
	// Any is any text feature on: the bool meaning.
	Any bool
}

// Level is one level from its bool shorthand (nil: not set) and its policy (nil: not set): the
// bool is the Default, and keys of the policy win over it.
func Level(on *bool, p *Policy) *Policy {
	if p == nil {
		if on == nil {
			return nil
		}
		return &Policy{Default: on}
	}
	if p.Default != nil || on == nil {
		return p
	}
	out := *p
	out.Default = on
	return &out
}

// Bool is the shorthand level for a bool: every feature on or off.
func Bool(on bool) *Policy { return &Policy{Default: &on} }

func walk(levels []*Policy, pick func(*Policy) *bool) *bool {
	for i := len(levels) - 1; i >= 0; i-- {
		if levels[i] == nil {
			continue
		}
		if v := pick(levels[i]); v != nil {
			return v
		}
	}
	return nil
}

func or(v ...*bool) *bool {
	for _, b := range v {
		if b != nil {
			return b
		}
	}
	return nil
}

func on(v *bool) bool { return v != nil && *v }

func spanKind(kind func(*SpanPolicy) *bool) func(*Policy) *bool {
	return func(p *Policy) *bool {
		if p.Spans == nil {
			return p.Default
		}
		return or(kind(p.Spans), p.Spans.Default, p.Default)
	}
}

// Resolve resolves levels, most general first (global then interaction, MCP wrapper or
// exporter): per key, the level's key, else its Default, else the level above; nothing set means
// off. IntentSummary and Spans.HTTP count only where set.
func Resolve(levels ...*Policy) Resolved {
	feature := func(f func(*Policy) *bool) bool {
		return on(walk(levels, func(p *Policy) *bool { return or(f(p), p.Default) }))
	}
	r := Resolved{
		AnswerText:       feature(func(p *Policy) *bool { return p.AnswerText }),
		LLMCalls:         feature(func(p *Policy) *bool { return p.LLMCalls }),
		FeedbackText:     feature(func(p *Policy) *bool { return p.FeedbackText }),
		ScoreExplanation: feature(func(p *Policy) *bool { return p.ScoreExplanation }),
		CallReason:       feature(func(p *Policy) *bool { return p.CallReason }),
		ErrorMessage:     feature(func(p *Policy) *bool { return p.ErrorMessage }),
		IntentSummary:    walk(levels, func(p *Policy) *bool { return p.IntentSummary }),
		Fallback:         on(walk(levels, func(p *Policy) *bool { return p.Default })),
		Spans: Spans{
			Messages:  on(walk(levels, spanKind(func(s *SpanPolicy) *bool { return s.Messages }))),
			ToolCalls: on(walk(levels, spanKind(func(s *SpanPolicy) *bool { return s.ToolCalls }))),
			HTTP: on(walk(levels, func(p *Policy) *bool {
				if p.Spans == nil {
					return nil
				}
				return p.Spans.HTTP
			})),
		},
	}
	r.Any = r.AnswerText || r.LLMCalls || r.FeedbackText || r.ScoreExplanation || r.CallReason ||
		r.ErrorMessage || on(r.IntentSummary) || r.Spans.Content()
	return r
}

// Field reports whether a content property (snake_case) is kept.
func (r Resolved) Field(name string) bool {
	switch name {
	case "prompt_text", "response_text":
		return r.AnswerText
	case "feedback_text", "capability_detail":
		return r.FeedbackText
	case "explanation":
		return r.ScoreExplanation
	case "intent_summary":
		if r.IntentSummary != nil {
			return *r.IntentSummary
		}
		return r.Fallback
	case "call_reason":
		return r.CallReason
	case "error_message":
		return r.ErrorMessage
	case "input_messages", "output_text", "output_tool_calls":
		return r.LLMCalls
	}
	return r.Any
}

// SendsIntentSummary reports whether intent_summary from the classifier or TrackClassification is
// sent: only an explicit IntentSummary false withholds it.
func (r Resolved) SendsIntentSummary() bool {
	return r.IntentSummary == nil || *r.IntentSummary
}
