package rudderstackai

import (
	"encoding/json"
	"maps"
	"os"
	"testing"
)

// spec/fixtures/mcp-agent-text-v0.1.json: call_reason, capability_detail and feedback_text as sent.
func TestMCPAgentTextFixture(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/mcp-agent-text-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		MaxChars map[string]int `json:"max_chars"`
		Cases    []struct {
			Name      string  `json:"name"`
			Field     string  `json:"field"`
			Redaction string  `json:"redaction"`
			Text      string  `json:"text"`
			Expected  *string `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	limits := map[string]int{"call_reason": maxTextLength, "capability_detail": maxTextLength, "feedback_text": feedbackMaxLength}
	if !maps.Equal(fixture.MaxChars, limits) {
		t.Fatalf("fixture max_chars %v, SDK %v", fixture.MaxChars, limits)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			o := &options{}
			o.DisableRedaction = tc.Redaction == "off"
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got := o.maskedText(tc.Text, fixture.MaxChars[tc.Field]); got != want {
				t.Fatalf("%s = %q, want %q", tc.Field, got, want)
			}
		})
	}
}
