package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bundled schemas are copies of the repository's spec, which go install cannot reach.
func TestBundledSchemasMatchSpec(t *testing.T) {
	spec := filepath.Join(repoRoot(t), "spec")
	for name, bundled := range map[string][]byte{
		"ai-events-v0.4.schema.json":        specSchema,
		"ai-client-events-v0.2.schema.json": clientSchema,
	} {
		b, err := os.ReadFile(filepath.Join(spec, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, bundled) {
			t.Errorf("schemas/%s differs from spec/%s: copy it again", name, name)
		}
	}
	doc, err := readSchema("", specSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(doc.id, "/"+specVersion) {
		t.Errorf("specVersion %s, bundled schema %s", specVersion, doc.id)
	}
}

func TestAcceptsMigrationSpecVersions(t *testing.T) {
	for _, version := range []string{"0_3_0", "0_4_0"} {
		t.Run(version, func(t *testing.T) {
			capture := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "spec_version_"+version+".jsonl")
			var stdout, stderr bytes.Buffer
			if code := run([]string{capture}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit code %d; stdout: %s; stderr: %s", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "context.wrapperLibrary.specVersion is") || !strings.Contains(stdout.String(), "RESULT: PASS") {
				t.Fatalf("unexpected checker output: %s", stdout.String())
			}
		})
	}
}

func TestBundledSchemaValidatesLimitEventSampleRate(t *testing.T) {
	spec, err := readSchema("", specSchema)
	if err != nil {
		t.Fatal(err)
	}
	client, err := readSchema("", clientSchema)
	if err != nil {
		t.Fatal(err)
	}
	sch, err := newSchemas(spec, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, rate := range []string{"0.1", "-0.1", "1.1", `"0.1"`} {
		t.Run(rate, func(t *testing.T) {
			v, err := parseJSON(`{"limit_type":"credits","interaction_id":"interaction-ü-✓","sample_rate":` + rate + `}`)
			if err != nil {
				t.Fatal(err)
			}
			fails, err := sch.errors(spec, "ai_limit_reached", v.(*Object))
			if err != nil {
				t.Fatal(err)
			}
			if valid := len(fails) == 0; valid != (rate == "0.1") {
				t.Fatalf("sample_rate %s: errors = %v", rate, fails)
			}
		})
	}
}

// testdata/values.expected holds, per line of testdata/values.txt, Python's
// [repr(v), json.dumps(v, sort_keys=True), json.dumps(v, sort_keys=True, indent=2)] for
// v = json.loads(line), or "error" when json.loads rejects the line (NaN and Infinity too).
func TestValuesAsPython(t *testing.T) {
	in, err := os.ReadFile("testdata/values.txt")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/values.expected")
	if err != nil {
		t.Fatal(err)
	}
	inputs, expected := strings.Split(string(in), "\n"), strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(inputs) != len(expected) {
		t.Fatalf("%d inputs, %d expected", len(inputs), len(expected))
	}
	for i, line := range inputs {
		v, err := parseJSON(line)
		if expected[i] == "error" {
			if err == nil {
				t.Errorf("%q: parsed as %s, want an error", line, repr(v))
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		var w [3]string
		if err := json.Unmarshal([]byte(expected[i]), &w); err != nil {
			t.Fatal(err)
		}
		if got := [3]string{repr(v), dumps(v, false), dumps(v, true)}; got != w {
			t.Errorf("%q:\n got %q\nwant %q", line, got, w)
		}
	}
}

func TestIntDigitLimit(t *testing.T) {
	if _, err := parseJSON(strings.Repeat("1", maxIntDigits)); err != nil {
		t.Error(err)
	}
	if _, err := parseJSON("-" + strings.Repeat("1", maxIntDigits+1)); err == nil {
		t.Error("an integer over Python's digit limit parsed")
	}
}

// testdata/keywords.stdout is the Python checker's output for the same capture and schema: one
// line per failed keyword of every kind the format names.
func TestSchemaKeywordsAsPython(t *testing.T) {
	want, err := os.ReadFile("testdata/keywords.stdout")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("testdata/keywords.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	// Without $schema, a schema is read as draft 2020-12.
	noDraft := filepath.Join(t.TempDir(), "nodraft.json")
	if err := os.WriteFile(noDraft, bytes.Replace(schema, []byte(`"$schema": "https://json-schema.org/draft/2020-12/schema",`), nil, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"testdata/keywords.schema.json", noDraft} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"testdata/keywords.jsonl", "--spec", path}, &stdout, &stderr)
		if code != 1 || stdout.String() != string(want) {
			t.Errorf("%s: exit %d, stderr %s, stdout:\n%s\nwant:\n%s", path, code, stderr.String(), stdout.String(), want)
		}
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	capture := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "pass.jsonl")
	notUTF8 := write("bad.jsonl", "{\"event\": \"x\xff\"}\n")
	notJSON := write("schema.txt", "{")
	array := write("array.json", "[]")
	noID := write("noid.json", `{"$defs": {}}`)
	invalid := write("invalid.json", `{"$id": "https://example.com/x", "$defs": {}, "type": 5}`)
	noDef := write("nodef.json", `{"$id": "https://example.com/y", "$defs": {}}`)
	noDefs := write("nodefs.json", `{"$id": "https://example.com/z"}`)
	empty := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "empty.jsonl")
	for _, c := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{capture, "-h"}, 0, ""},
		{[]string{"what-changes", "--help"}, 0, ""},
		{[]string{notUTF8}, 2, "cannot read file"},
		{[]string{dir}, 2, "cannot read file"},
		{[]string{"what-changes", capture, notUTF8}, 2, "cannot read file"},
		{[]string{capture, "--spec", filepath.Join(dir, "missing.json")}, 2, "cannot read schema"},
		{[]string{capture, "--spec", notJSON}, 2, "cannot read schema"},
		{[]string{capture, "--spec", array}, 2, "cannot read schema"},
		{[]string{capture, "--client-schema", noID}, 2, "cannot read schema"},
		{[]string{capture, "--spec", invalid}, 2, "cannot read schema"},
		{[]string{empty, "--no-ai-events", "--spec", noDefs}, 2, noDefs + ": cannot read schema"},
		{[]string{empty, "--no-ai-events", "--client-schema", noDefs}, 2, noDefs + ": cannot read schema"},
		{[]string{capture, "--spec", noDef}, 2, "cannot read schema"},
		{[]string{capture, "--expect-joins=1"}, 2, "ignored explicit argument"},
		{[]string{capture, "--client-opt-out=x"}, 2, "invalid choice"},
		{[]string{capture, capture}, 2, "unrecognized arguments"},
		{[]string{}, 2, "required: events"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(c.args, &stdout, &stderr)
		if code != c.code || !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("%v: exit %d, stderr %q", c.args, code, stderr.String())
		}
		if c.code == 0 && stdout.String() != usage {
			t.Errorf("%v: stdout %q", c.args, stdout.String())
		}
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args        []string
		positionals []string
		feature     []string
		ok          bool
	}{
		{[]string{"--", "a"}, []string{"a"}, nil, true},
		{[]string{"a", "--"}, []string{"a"}, nil, true},
		{[]string{"--", "--"}, []string{"--"}, nil, true},
		{[]string{"--feature", "-1", "a"}, []string{"a"}, []string{"-1"}, true},
		{[]string{"--feature=x=y", "-", "--feature", "z"}, []string{"-"}, []string{"x=y", "z"}, true},
		{[]string{"--weird value", "--feature", ""}, []string{"--weird value"}, []string{""}, true},
		{[]string{"a", "--feature", "-x"}, nil, nil, false},
		{[]string{"a", "--expect-joins", "--"}, nil, nil, false},
		{[]string{"a", "-x"}, nil, nil, false},
		{[]string{"--x=1", "a"}, nil, nil, false},
	} {
		p, err := parseArgs(c.args, []string{"events"}, checkFlags)
		if (err == nil) != c.ok {
			t.Errorf("%q: error %v", c.args, err)
			continue
		}
		if err == nil && (strings.Join(p.positionals, "|") != strings.Join(c.positionals, "|") ||
			strings.Join(p.lists["feature"], "|") != strings.Join(c.feature, "|")) {
			t.Errorf("%q: positionals %q, feature %q", c.args, p.positionals, p.lists["feature"])
		}
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []any{nil, false, Int("0"), Float(0), "", []any{}, newObject()} {
		if truthy(v) {
			t.Errorf("%s is truthy", repr(v))
		}
	}
	o := newObject()
	o.Set("k", nil)
	for _, v := range []any{true, Int("-1"), Float(0.5), "0", []any{nil}, o} {
		if !truthy(v) {
			t.Errorf("%s is falsy", repr(v))
		}
	}
	if text(Int("7")) != "7" || text("x") != "x" || typeName(true) != "bool" || typeName(Float(1)) != "float" {
		t.Error("text or typeName")
	}
}

// Mirrors Python's test_invalid_schema_override_exits_2.
func TestInvalidSchemaOverrideExits2(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad-pattern.json")
	if err := os.WriteFile(bad, []byte(`{"$id": "https://example.com/c", "$defs": {"ai_user_action": {"pattern": "["}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "empty.jsonl")
	var stdout, stderr bytes.Buffer
	code := run([]string{empty, "--no-ai-events", "--client-schema", bad}, &stdout, &stderr)
	if code != 2 || stdout.String() != "" || stderr.String() != "error: "+bad+": cannot read schema\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// Mirrors Python's test_additional_properties_skip_pattern_properties.
func TestAdditionalPropertiesSkipPatternProperties(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "pattern.json")
	capture := filepath.Join(dir, "c.jsonl")
	if err := os.WriteFile(schema, []byte(`{"$id": "https://example.com/p", "$defs": {"ai_user_action": {`+
		`"properties": {"source": {}, "a": {}}, "patternProperties": {"^x": {"type": "integer"}}, "additionalProperties": false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capture, []byte(`{"event": "ai_user_action", "properties": {"source": "client", "xa": "s", "yb": 1}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	run([]string{capture, "--client-schema", schema}, &stdout, &stderr)
	var fails []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "(client schema)") {
			fails = append(fails, line)
		}
	}
	want := []string{
		"FAIL: #0 ai_user_action (client schema): properties.xa: not of type integer",
		"FAIL: #0 ai_user_action (client schema): properties: unexpected 'yb'",
	}
	if strings.Join(fails, "\n") != strings.Join(want, "\n") {
		t.Errorf("client schema lines:\n%s", strings.Join(fails, "\n"))
	}
}

// Mirrors Python's test_unresolvable_schema_override_exits_2: a missing definition or a dangling $ref.
func TestUnresolvableSchemaOverrideExits2(t *testing.T) {
	capture := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "pass.jsonl")
	for _, c := range []struct{ flag, defs string }{
		{"--spec", `{}`},
		{"--client-schema", `{"ai_user_action": {"$ref": "#/$defs/missing"}}`},
	} {
		schema := filepath.Join(t.TempDir(), "schema.json")
		if err := os.WriteFile(schema, []byte(`{"$id": "https://example.com/s", "$defs": `+c.defs+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{capture, c.flag, schema}, &stdout, &stderr)
		if code != 2 || stdout.String() != "" || stderr.String() != "error: "+schema+": cannot read schema\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.flag, code, stdout.String(), stderr.String())
		}
	}
}

func TestMCPIdentityFlagsAreExclusive(t *testing.T) {
	capture := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "pass.jsonl")
	var stdout, stderr bytes.Buffer
	if code := run([]string{capture, "--mcp-anonymous", "--mcp-user-id", "u1"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q", code, stdout.String())
	}
}

// Mirrors Python's test_schema_override_in_another_dialect_exits_2.
func TestSchemaOverrideInAnotherDialectExits2(t *testing.T) {
	empty := filepath.Join(repoRoot(t), "spec", "fixtures", "migration-check", "empty.jsonl")
	for _, d := range []string{`"http://json-schema.org/draft-07/schema#"`, `"foo"`, `null`} {
		schema := filepath.Join(t.TempDir(), "schema.json")
		if err := os.WriteFile(schema, []byte(`{"$schema": `+d+`, "$id": "https://example.com/s", "$defs": {}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{empty, "--no-ai-events", "--client-schema", schema}, &stdout, &stderr)
		if code != 2 || stdout.String() != "" || stderr.String() != "error: "+schema+": cannot read schema\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", d, code, stdout.String(), stderr.String())
		}
	}
	for _, d := range []string{`"https://json-schema.org/draft/2020-12/schema#"`} {
		schema := filepath.Join(t.TempDir(), "schema.json")
		if err := os.WriteFile(schema, []byte(`{"$schema": `+d+`, "$id": "https://example.com/s", "$defs": {}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{empty, "--no-ai-events", "--client-schema", schema}, &stdout, &stderr); code != 0 {
			t.Errorf("%s: exit %d, stderr %q", d, code, stderr.String())
		}
	}
}
