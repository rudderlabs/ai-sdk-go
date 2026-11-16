package spanmodel

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The content lists match the shared list every SDK checks.
func TestContentKeysMatchSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../spec/fixtures/span-content-keys-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		AttributeKeys       []string            `json:"attributeKeys"`
		AttributePrefixes   []string            `json:"attributePrefixes"`
		ScopedAttributeKeys map[string][]string `json:"scopedAttributeKeys"`
		EventNames          []string            `json:"eventNames"`
		ToolCallKeys        []string            `json:"toolCallAttributeKeys"`
		HTTPKeys            []string            `json:"httpAttributeKeys"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(contentKeys, fixture.AttributeKeys) {
		t.Errorf("content keys %v, want %v", contentKeys, fixture.AttributeKeys)
	}
	if !reflect.DeepEqual(contentPrefixes, fixture.AttributePrefixes) {
		t.Errorf("content prefixes %v, want %v", contentPrefixes, fixture.AttributePrefixes)
	}
	if !reflect.DeepEqual(scopedContentKeys, fixture.ScopedAttributeKeys) {
		t.Errorf("scoped content keys %v, want %v", scopedContentKeys, fixture.ScopedAttributeKeys)
	}
	if !reflect.DeepEqual(toolCallKeys, fixture.ToolCallKeys) {
		t.Errorf("tool call keys %v, want %v", toolCallKeys, fixture.ToolCallKeys)
	}
	if !reflect.DeepEqual(httpKeys, fixture.HTTPKeys) {
		t.Errorf("http keys %v, want %v", httpKeys, fixture.HTTPKeys)
	}
	for _, name := range fixture.EventNames {
		if !contentEvent(name) {
			t.Errorf("event %q is not content", name)
		}
	}
}

func TestScopedContentKeys(t *testing.T) {
	if !contentAttribute("prompt", "pydantic-ai") {
		t.Error("prompt on pydantic-ai spans is content")
	}
	if contentAttribute("prompt", "") || contentAttribute("prompt", "other") {
		t.Error("prompt on other spans is not content")
	}
}
