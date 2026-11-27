package testkit

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ToolOutputCase is one case of spec/fixtures/tool-output-failure-v0.1.json. A JSON string
// output stays a string. FrameworkFailure: the adapter's own failure flag is set, with no category.
type ToolOutputCase struct {
	Name             string `json:"name"`
	FrameworkFailure bool   `json:"framework_failure"`
	Output           any    `json:"output"`
	Expected         struct {
		Error     bool   `json:"error"`
		ErrorType string `json:"error_type"`
	} `json:"expected"`
}

// ToolOutputCases loads the shared tool output failure fixture.
func ToolOutputCases() []ToolOutputCase {
	raw, err := os.ReadFile(filepath.Join(RepoRoot(), "spec", "fixtures", "tool-output-failure-v0.1.json"))
	if err != nil {
		panic(err)
	}
	var f struct {
		Cases []ToolOutputCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Cases) == 0 {
		panic("testkit: bad tool-output-failure fixture")
	}
	return f.Cases
}
