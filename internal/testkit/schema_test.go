package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoRoot(t *testing.T) {
	root := RepoRoot()
	if _, err := os.Stat(filepath.Join(root, "spec", "ai-events-v0.3.schema.json")); err != nil {
		t.Fatal(err)
	}
}

func TestFindRootWithoutGoMod(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	findRoot(string(filepath.Separator))
}

func TestValidateEvent(t *testing.T) {
	ids := map[string]any{"conversation_id": "c1", "interaction_id": "i1"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range ids {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	valid := []struct {
		event string
		props map[string]any
	}{
		{"ai_user_action", with(map[string]any{"action_type": "copied_response"})},
		{"ai_response_received", with(map[string]any{"response_status": "success"})},
		{"order_looked_up", with(map[string]any{"tool_name": "lookup_order", "tool_status": "success"})},
	}
	for _, c := range valid {
		if err := ValidateEvent(c.event, c.props); err != nil {
			t.Errorf("%s: %v", c.event, err)
		}
	}
	invalid := []struct {
		event string
		props map[string]any
		want  string
	}{
		{"ai_user_action", ids, "ai_user_action"},
		{"ai_response_received", with(map[string]any{"response_status": 7}), "ai_response_received"},
		{"order_looked_up", ids, "order_looked_up"},
		{"ai_not_an_event", ids, `no schema for reserved event "ai_not_an_event"`},
		{"mcp_fields", ids, `no schema for reserved event "mcp_fields"`},
		{"ai_user_action", map[string]any{"bad": func() {}}, "unsupported type"},
	}
	for _, c := range invalid {
		err := ValidateEvent(c.event, c.props)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: err = %v, want %q", c.event, c.props, err, c.want)
		}
	}
}

func TestLoadSchemasErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadSchemas(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file loaded")
	}
	for name, body := range map[string]string{
		"bad.json":     "{",
		"badref.json":  `{"$defs":{"ai_x":{"allOf":[{"$ref":"#/$defs/nope"}]}}}`,
		"baddefs.json": `{"$defs":[]}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSchemas(p); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
}
