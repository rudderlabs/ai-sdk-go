package capture

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type fixture struct {
	Resolution []struct {
		Name          string            `json:"name"`
		NotApplicable map[string]string `json:"not_applicable"`
		Levels        []level           `json:"levels"`
		Expected      struct {
			AnswerText       bool  `json:"answer_text"`
			LLMCalls         bool  `json:"llm_calls"`
			FeedbackText     bool  `json:"feedback_text"`
			ScoreExplanation bool  `json:"score_explanation"`
			CallReason       bool  `json:"call_reason"`
			ErrorMessage     bool  `json:"error_message"`
			IntentSummary    *bool `json:"intent_summary"`
			Spans            struct {
				Messages  bool `json:"messages"`
				ToolCalls bool `json:"tool_calls"`
				HTTP      bool `json:"http"`
			} `json:"spans"`
		} `json:"expected"`
	} `json:"resolution"`
	CustomProperties []struct {
		Name          string            `json:"name"`
		NotApplicable map[string]string `json:"not_applicable"`
		Levels        []level           `json:"levels"`
		Property      string            `json:"property"`
		Sent          bool              `json:"sent"`
	} `json:"custom_properties"`
	// Invalid policies: Node.js and Python reject them; Go's types cannot express them.
	Invalid           []invalidCase `json:"invalid"`
	InvalidAtCallSite []invalidCase `json:"invalid_at_call_site"`
}

type invalidCase struct {
	Name          string            `json:"name"`
	NotApplicable map[string]string `json:"not_applicable"`
}

// notApplicable skips a case the fixture marks not applicable to Go, with its reason.
func notApplicable(t *testing.T, na map[string]string) {
	t.Helper()
	if reason, ok := na["go"]; ok {
		t.Skip("N/A for Go: " + reason)
	}
}

type level struct {
	CaptureContent json.RawMessage `json:"capture_content"`
}

func load(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("../../spec/fixtures/capture-policy-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// FixturePolicy is a fixture level as a Go level. ok is false for values Go's types cannot hold
// (strings, numbers: Node and Python coerce them).
func FixturePolicy(raw json.RawMessage) (p *Policy, ok bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return Bool(b), true, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false, nil
	}
	p = &Policy{}
	fields := map[string]**bool{
		"default": &p.Default, "answer_text": &p.AnswerText, "llm_calls": &p.LLMCalls,
		"feedback_text": &p.FeedbackText, "score_explanation": &p.ScoreExplanation,
		"intent_summary": &p.IntentSummary, "call_reason": &p.CallReason, "error_message": &p.ErrorMessage,
	}
	for k, v := range m {
		if k == "spans" {
			var on bool
			if json.Unmarshal(v, &on) == nil {
				p.Spans = &SpanPolicy{Default: &on}
				continue
			}
			var s struct {
				Default   *bool `json:"default"`
				Messages  *bool `json:"messages"`
				ToolCalls *bool `json:"tool_calls"`
				HTTP      *bool `json:"http"`
			}
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, false, err
			}
			p.Spans = &SpanPolicy{Default: s.Default, Messages: s.Messages, ToolCalls: s.ToolCalls, HTTP: s.HTTP}
			continue
		}
		var on *bool
		if err := json.Unmarshal(v, &on); err != nil {
			return nil, false, err
		}
		*fields[k] = on
	}
	return p, true, nil
}

func levels(t *testing.T, in []level) ([]*Policy, bool) {
	t.Helper()
	out := make([]*Policy, len(in))
	for i, l := range in {
		p, ok, err := FixturePolicy(l.CaptureContent)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return nil, false
		}
		out[i] = p
	}
	return out, true
}

func TestResolutionFixture(t *testing.T) {
	for _, c := range load(t).Resolution {
		t.Run(c.Name, func(t *testing.T) {
			notApplicable(t, c.NotApplicable)
			ls, ok := levels(t, c.Levels)
			if !ok {
				// Coercion cases (strings, numbers): Node.js and Python read them with Boolean()/bool().
				t.Skip("N/A for Go: a non-boolean, non-object level; Go's types cannot hold it")
			}
			r := Resolve(ls...)
			e := c.Expected
			got := []any{r.AnswerText, r.LLMCalls, r.FeedbackText, r.ScoreExplanation, r.CallReason, r.ErrorMessage, r.IntentSummary, r.Spans}
			want := []any{e.AnswerText, e.LLMCalls, e.FeedbackText, e.ScoreExplanation, e.CallReason, e.ErrorMessage, e.IntentSummary, Spans{e.Spans.Messages, e.Spans.ToolCalls, e.Spans.HTTP}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("resolved %v, want %v", got, want)
			}
		})
	}
}

func TestCustomPropertiesFixture(t *testing.T) {
	for _, c := range load(t).CustomProperties {
		t.Run(c.Name, func(t *testing.T) {
			notApplicable(t, c.NotApplicable)
			ls, ok := levels(t, c.Levels)
			if !ok {
				t.Fatal("a non-boolean, non-object level without not_applicable for go")
			}
			if got := Resolve(ls...).Field(c.Property); got != c.Sent {
				t.Fatalf("%s kept = %v, want %v", c.Property, got, c.Sent)
			}
		})
	}
}

func TestLevelBoolIsDefault(t *testing.T) {
	yes, no := true, false
	if got := Resolve(Level(&yes, nil)); !got.AnswerText || !got.Spans.Messages || got.Spans.HTTP || got.IntentSummary != nil {
		t.Fatalf("bool true: %+v", got)
	}
	// Keys of the policy win over the bool shorthand at the same level.
	got := Resolve(Level(&yes, &Policy{CallReason: &no}))
	if got.CallReason || !got.AnswerText {
		t.Fatalf("bool true + call_reason false: %+v", got)
	}
	if got := Resolve(Level(&yes, &Policy{Default: &no})); got.AnswerText {
		t.Fatalf("policy default wins over the bool: %+v", got)
	}
	if Level(nil, nil) != nil {
		t.Fatal("nothing set is nil")
	}
}

// Per-call content names in custom properties follow LLMCalls, the bool form included.
func TestCallContentPropertiesFollowLLMCalls(t *testing.T) {
	yes, no := true, false
	for _, name := range []string{"input_messages", "output_text", "output_tool_calls"} {
		if Resolve(Bool(false)).Field(name) || !Resolve(Bool(true)).Field(name) {
			t.Errorf("%s does not follow the bool", name)
		}
		if Resolve(&Policy{Default: &yes, LLMCalls: &no}).Field(name) || !Resolve(&Policy{LLMCalls: &yes}).Field(name) {
			t.Errorf("%s does not follow LLMCalls", name)
		}
	}
}

// Resolved values and clones never share the caller's pointers.
func TestResolveAndCloneCopyValues(t *testing.T) {
	on := true
	p := &Policy{IntentSummary: &on, Spans: &SpanPolicy{HTTP: &on}}
	r, c := Resolve(p), Clone(p)
	on = false
	if r.IntentSummary == nil || !*r.IntentSummary {
		t.Fatal("resolved intent_summary follows the caller's value")
	}
	if !*c.IntentSummary || !*c.Spans.HTTP {
		t.Fatal("clone follows the caller's values")
	}
	if Clone(nil) != nil {
		t.Fatal("clone of nil")
	}
}

// Every invalid case must be marked not applicable to Go: none is ignored.
func TestInvalidFixtureCasesNotApplicable(t *testing.T) {
	f := load(t)
	for _, c := range append(f.Invalid, f.InvalidAtCallSite...) {
		t.Run(c.Name, func(t *testing.T) {
			notApplicable(t, c.NotApplicable)
			t.Fatal("invalid case without not_applicable for go: Go has no way to run it")
		})
	}
}
