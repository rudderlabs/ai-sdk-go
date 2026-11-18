// Package serializerfixture resolves the shared serializer fixture's Go inputs.
package serializerfixture

import (
	"fmt"
	"maps"
	"slices"
)

// Case is a shared serializer-capture fixture case.
type Case struct {
	ID        string   `json:"id"`
	AppliesTo []string `json:"applies_to"`
	Config    struct {
		CaptureContent bool `json:"capture_content"`
		EmitSpans      bool `json:"emit_spans"`
		Redact         bool `json:"redact"`
	} `json:"config"`
	Setup       map[string]any            `json:"setup"`
	Expect      map[string]any            `json:"expect"`
	ExpectBySDK map[string]map[string]any `json:"expect_by_sdk"`
}

// AppliesToGo reports whether the case must run in Go.
func (c Case) AppliesToGo() bool { return slices.Contains(c.AppliesTo, "go") }

// ResolveGo shallowly replaces observations and selects the Go report sinks.
// Clone maps so resolution does not change the shared default expectations.
func (c Case) ResolveGo() (Case, error) {
	c.Expect = maps.Clone(c.Expect)
	for key, value := range c.ExpectBySDK["go"] {
		c.Expect[key] = value
	}
	c.Setup = maps.Clone(c.Setup)
	if bySDK, ok := c.Setup["sinks_by_sdk"].(map[string]any); ok {
		if sinks, ok := bySDK["go"]; ok {
			c.Setup["sinks"] = sinks
		}
	}
	if c.Setup["scenario"] == "report_sinks" {
		sinks, ok := c.Setup["sinks"].([]any)
		if !ok {
			return c, fmt.Errorf("missing report sinks (%s)", c.ID)
		}
		decoded, ok := c.Expect["decoded"].(map[string]any)
		if !ok {
			return c, fmt.Errorf("missing decoded sink expectations (%s)", c.ID)
		}
		selected := make(map[string]any, len(sinks))
		for _, sink := range sinks {
			name, ok := sink.(string)
			if !ok {
				return c, fmt.Errorf("invalid report sink %v (%s)", sink, c.ID)
			}
			value, ok := decoded[name]
			if !ok {
				return c, fmt.Errorf("unknown sink expectation %q (%s)", name, c.ID)
			}
			selected[name] = value
		}
		c.Expect["decoded"] = selected
	}
	return c, nil
}
