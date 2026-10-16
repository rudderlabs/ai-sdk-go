package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://rudderstack.com/schemas/ai-events/0.3.0"

// RepoRoot returns the nearest directory at or above the working directory that holds go.mod.
// It panics when there is none: tests always run inside the module.
func RepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return findRoot(dir)
}

func findRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("testkit: no go.mod above " + dir)
		}
		dir = parent
	}
}

var (
	schemaOnce sync.Once
	schemas    map[string]*jsonschema.Schema
	schemaErr  error
)

// loadSchemas compiles every event definition of the spec schema: the `$defs` entries built with
// allOf (the shared fragments such as usage_entry or mcp_fields are not events).
func loadSchemas(path string) (map[string]*jsonschema.Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Defs map[string]map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	out := map[string]*jsonschema.Schema{}
	for name, def := range raw.Defs {
		if _, ok := def["allOf"]; !ok {
			continue
		}
		if out[name], err = c.Compile(schemaURL + "#/$defs/" + name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ValidateEvent checks an event's properties against spec/ai-events-v0.3.schema.json: reserved
// names (ai_*, mcp_*) against `$defs/<event>`, customer-named events against
// `$defs/declared_tool_event`. A reserved name with no definition is an error.
func ValidateEvent(event string, properties map[string]any) error {
	schemaOnce.Do(func() {
		schemas, schemaErr = loadSchemas(filepath.Join(RepoRoot(), "spec", "ai-events-v0.3.schema.json"))
	})
	if schemaErr != nil {
		return schemaErr
	}
	def := event
	if !strings.HasPrefix(event, "ai_") && !strings.HasPrefix(event, "mcp_") {
		def = "declared_tool_event"
	}
	s, ok := schemas[def]
	if !ok {
		return fmt.Errorf("no schema for reserved event %q", event)
	}
	b, err := json.Marshal(properties)
	if err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	if err := s.Validate(v); err != nil {
		return fmt.Errorf("%s: %w", event, err)
	}
	return nil
}
