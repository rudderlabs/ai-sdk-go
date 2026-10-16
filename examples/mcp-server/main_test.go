package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) == "1" {
		os.Exit(run(os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

func TestCheckPassesAndEventsMatchTheSchema(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--check"}, &out); code != 0 {
		t.Fatalf("check exited %d:\n%s", code, out.String())
	}
	f, err := os.Open("../../spec/ai-events-v0.3.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://rudderstack.com/schemas/ai-events/0.3.0"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	for _, m := range lastMessages {
		def := m.Event
		if !strings.HasPrefix(def, "mcp_") {
			def = "mcp_declared_tool_event"
		}
		schema, err := c.Compile(url + "#/$defs/" + def)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(m.Properties)
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err := schema.Validate(v); err != nil {
			t.Errorf("%s: %v", m.Event, err)
		}
	}
	if len(lastMessages) < 15 {
		t.Errorf("only %d events", len(lastMessages))
	}
}

func TestRunErrors(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--nope"}, &out); code != 2 {
		t.Errorf("bad flag exit = %d", code)
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	if code := run([]string{"--transport=http"}, &out); code != 1 || !strings.Contains(out.String(), "RUDDERSTACK_WRITE_KEY") {
		t.Errorf("missing env exit = %d: %s", code, out.String())
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "k")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", "http://127.0.0.1:1")
	if code := run([]string{"--transport=carrier-pigeon"}, &out); code != 1 {
		t.Errorf("bad transport exit = %d", code)
	}
}
