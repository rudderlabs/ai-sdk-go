package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

// capturePolicy is the plan's capture_content as the checker reads it: --capture-content (the
// level above) and --capture-policy (snake_case keys, as in the AI events plan), resolved like the
// SDKs' captureContent policy (SDK-6888). nil fields are not set.
type capturePolicy struct {
	on     bool
	policy map[string]*bool
}

var policyKeys = []string{"default", "answer_text", "llm_calls", "spans", "feedback_text", "score_explanation", "intent_summary", "call_reason", "error_message"}
var spanKeys = []string{"default", "messages", "tool_calls", "http"}

// parseCapturePolicy reads --capture-policy; an empty value is no policy.
func parseCapturePolicy(raw string) (map[string]*bool, error) {
	if raw == "" {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return nil, usageErr("argument --capture-policy: must be a JSON object")
	}
	out := map[string]*bool{}
	for k, v := range m {
		if !slices.Contains(policyKeys, k) {
			return nil, usageErr("argument --capture-policy: captureContent.%s is not a captureContent option", k)
		}
		if k == "spans" {
			// Span content is not on events the checker reads; only its shape is checked.
			var b bool
			var s map[string]*bool
			if json.Unmarshal(v, &b) == nil {
				continue
			}
			if json.Unmarshal(v, &s) != nil || s == nil {
				return nil, usageErr("argument --capture-policy: captureContent.spans must be a boolean or an object")
			}
			for sk := range s {
				if !slices.Contains(spanKeys, sk) {
					return nil, usageErr("argument --capture-policy: captureContent.spans.%s is not a captureContent option", sk)
				}
			}
			continue
		}
		var b *bool
		if json.Unmarshal(v, &b) != nil {
			return nil, usageErr("argument --capture-policy: captureContent.%s must be a boolean", k)
		}
		out[k] = b
	}
	return out, nil
}

func (c capturePolicy) key(name string) bool {
	if v := c.policy[name]; v != nil {
		return *v
	}
	if v := c.policy["default"]; v != nil {
		return *v
	}
	return c.on
}

// keeps reports whether a content property may be sent under the plan.
func (c capturePolicy) keeps(field string) bool {
	switch field {
	case "prompt_text", "response_text":
		return c.key("answer_text")
	case "feedback_text", "capability_detail":
		return c.key("feedback_text")
	case "explanation":
		return c.key("score_explanation")
	case "call_reason", "error_message":
		return c.key(field)
	}
	panic(fmt.Sprintf("not a content field: %s", field))
}

// intentSummaryOff: only an explicit intent_summary false withholds the classifier's summary.
func (c capturePolicy) intentSummaryOff() bool {
	v := c.policy["intent_summary"]
	return v != nil && !*v
}
