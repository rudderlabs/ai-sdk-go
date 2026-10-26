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
		"ai-events-v0.3.schema.json":        specSchema,
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
		{[]string{capture, "--spec", invalid}, 2, "cannot compile"},
		{[]string{empty, "--no-ai-events", "--spec", noDefs}, 2, noDefs + ": cannot read schema"},
		{[]string{empty, "--no-ai-events", "--client-schema", noDefs}, 2, noDefs + ": cannot read schema"},
		{[]string{capture, "--spec", noDef}, 2, "cannot compile"},
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
