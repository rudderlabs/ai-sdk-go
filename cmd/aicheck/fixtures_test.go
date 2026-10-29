package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the nearest directory above the working directory that holds spec/.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "spec", "fixtures", "migration-check-v0.1.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no spec/fixtures/migration-check-v0.1.json above the working directory")
		}
		dir = parent
	}
}

type fixtureCase struct {
	Name     string   `json:"name"`
	Command  string   `json:"command"`
	Captures []string `json:"captures"`
	Args     []string `json:"args"`
	ExitCode int      `json:"exit_code"`
	Stdout   *string  `json:"stdout"`
}

// TestFixtures runs every case of spec/fixtures/migration-check-v0.1.json (shared with the Node.js
// and Python checkers) and compares the exit code and stdout.
func TestFixtures(t *testing.T) {
	fixtures := filepath.Join(repoRoot(t), "spec", "fixtures")
	b, err := os.ReadFile(filepath.Join(fixtures, "migration-check-v0.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []fixtureCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var args []string
			if c.Command == "what-changes" {
				args = append(args, "what-changes")
			}
			for _, capture := range c.Captures {
				args = append(args, filepath.Join(fixtures, "migration-check", capture+".jsonl"))
			}
			args = append(args, c.Args...)
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != c.ExitCode {
				t.Errorf("exit code %d, want %d; stderr:\n%s", code, c.ExitCode, stderr.String())
			}
			if c.Stdout != nil && stdout.String() != *c.Stdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", stdout.String(), *c.Stdout)
			}
		})
	}
}

func TestDocumentsEveryStandardUserAction(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "spec", "ai-events-v0.4.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	actions := schema["$defs"].(map[string]any)["ai_user_action"].(map[string]any)["properties"].(map[string]any)["action_type"].(map[string]any)["enum"].([]any)
	docs, err := os.ReadFile(filepath.Join(root, "docs", "events.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if !strings.Contains(string(docs), "`"+action.(string)+"`") {
			t.Errorf("undocumented action: %s", action)
		}
	}
}
