package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewCheck(t *testing.T, capture string, args ...string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	if err := os.WriteFile(path, []byte(capture), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := run(append([]string{path}, args...), &out, &stderr)
	if stderr.Len() > 0 {
		t.Fatal(stderr.String())
	}
	return code, out.String()
}

func TestReviewFeedbackSystemTrigger(t *testing.T) {
	capture := `{"type":"track","event":"ai_feedback_requested","userId":"u1","properties":{"source":"client","feature":"support_bot","surface":"mobile","conversation_id":"c1","interaction_id":"11111111-1111-4111-8111-111111111111","trigger":"system"}}`
	if code, out := reviewCheck(t, capture); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestReviewJoinedToolMissingName(t *testing.T) {
	capture := `{"type":"track","event":"run_query","userId":"u1","properties":{"source":"client","conversation_id":"c1","interaction_id":"11111111-1111-4111-8111-111111111111","tool_status":"success"}}`
	code, out := reviewCheck(t, capture, "--expect-legacy", "run_query", "--no-ai-events")
	if code != 1 || !strings.Contains(out, "tool_name") || !strings.Contains(out, "AI events") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestReviewActionScopeRequiresAnswer(t *testing.T) {
	capture := `{"type":"track","event":"ai_user_action","userId":"u1","properties":{"source":"client","conversation_id":"c1","interaction_id":"11111111-1111-4111-8111-111111111111","action_type":"accepted","action_details":{"feedback_scope":"feature"}}}`
	if code, out := reviewCheck(t, capture, "--expect-joins"); code != 1 || !strings.Contains(out, "for an answer not in this capture") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestReviewClientToolRows(t *testing.T) {
	capture, err := os.ReadFile(filepath.Join(repoRoot(t), "spec/fixtures/migration-check/client_tool_legacy_valid_migration.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(capture)), "\n")
	for index, want := range map[int]string{2: "run_query", 3: "run_query (browser)"} {
		value, err := parseJSON(lines[index])
		if err != nil {
			t.Fatal(err)
		}
		if got := rowKey(obj(value)); got != want {
			t.Errorf("row %d = %s, want %s", index, got, want)
		}
	}
}

func TestReviewCatalogDestinationLimit(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "spec/rudderstack-catalog/properties.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "    - id: destination\n")
	if !found {
		t.Fatal("missing destination")
	}
	section, _, _ = strings.Cut(section, "    - id:")
	if !strings.Contains(section, "max_length: 64") || strings.Contains(section, "maxLength:") {
		t.Fatal(section)
	}
}

func TestReviewClientSchemaOverrideName(t *testing.T) {
	schema := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(schema, []byte(`{"$id":"https://example.com/client","$defs":{"custom_browser_event":{"type":"object","required":["source","required_value"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	capture := `{"type":"track","event":"custom_browser_event","userId":"u","properties":{"source":"client"}}`
	if code, out := reviewCheck(t, capture, "--client-schema", schema); code != 1 || !strings.Contains(out, "required_value") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestReviewConversationForbiddenFields(t *testing.T) {
	for _, event := range []string{"ai_conversation_pinned", "ai_conversation_shared"} {
		fields := []string{"title", "name"}
		if event == "ai_conversation_shared" {
			fields = append(fields, "share_link")
		}
		for _, field := range fields {
			capture := `{"type":"track","event":"` + event + `","userId":"u","properties":{"source":"client","conversation_id":"c","` + field + `":"secret"}}`
			if code, out := reviewCheck(t, capture); code != 1 {
				t.Errorf("%s %s: exit %d: %s", event, field, code, out)
			}
		}
	}
}

func TestReviewClientHelperNamesAreDeclaredTools(t *testing.T) {
	for _, name := range []string{"common", "with_conversation", "with_interaction"} {
		capture := `{"type":"track","event":"` + name + `","userId":"u1","properties":{"source":"client","conversation_id":"c1","interaction_id":"11111111-1111-4111-8111-111111111111","tool_name":"` + name + `","arguments":{"q":1}}}`
		code, out := reviewCheck(t, capture)
		if code != 1 || !strings.Contains(out, "tool_status") || !strings.Contains(out, "must not") {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
	}
}

func TestReviewLimitRows(t *testing.T) {
	for _, tc := range []struct{ capture, want string }{
		{`{"event":"ai_limit_reached","properties":{"source":"client"}}`, "ai_limit_reached (browser)"},
		{`{"event":"ai_limit_reached","properties":{},"context":{"wrapperLibrary":{"name":"ai-sdk-go"}}}`, "ai_limit_reached"},
	} {
		v, err := parseJSON(tc.capture)
		if err != nil {
			t.Fatal(err)
		}
		if got := rowKey(obj(v)); got != tc.want {
			t.Fatalf("got %s, want %s", got, tc.want)
		}
	}
}

func TestReviewZeroSampleRate(t *testing.T) {
	var out, stderr bytes.Buffer
	capture := `{"type":"track","event":"ai_limit_reached","userId":"u","properties":{"limit_type":"credits","sample_rate":0},"context":{"wrapperLibrary":{"name":"rudderstack-ai","version":"0.7.0","specVersion":"0.3.0","integration":"manual"}}}`
	value, err := parseJSON(capture)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := readSchema("", specSchema)
	if err != nil {
		t.Fatal(err)
	}
	client, err := readSchema("", clientSchema)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := newSchemas(spec, client)
	if err != nil {
		t.Fatal(err)
	}
	code, err := check([]*Object{obj(value)}, schemas, options{}, &out)
	if err != nil || code != 1 || !strings.Contains(out.String(), "sample_rate") {
		t.Fatal(code, err, out.String(), stderr.String())
	}
}

func TestReviewSampleRateCatalog(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "spec/rudderstack-catalog/properties.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(b), "    - id: sample_rate\n")
	if !ok {
		t.Fatal("missing sample rate")
	}
	section, _, _ = strings.Cut(section, "    - id:")
	if !strings.Contains(section, "exclusive_minimum: 0") || strings.Contains(section, "        minimum: 0") {
		t.Fatal(section)
	}
}
