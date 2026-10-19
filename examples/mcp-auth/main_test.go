package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

func TestCheck(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--check"}, &out); code != 0 {
		t.Fatalf("check exited %d:\n%s", code, out.String())
	}
}

// TestLive runs --live against a mock standing in for the real data plane.
func TestLive(t *testing.T) {
	upstream := dataplane.Start()
	defer upstream.Close()
	t.Setenv("RUDDERSTACK_WRITE_KEY", "live-key")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", upstream.URL)
	var out bytes.Buffer
	if code := run([]string{"--live"}, &out); code != 0 {
		t.Fatalf("live check exited %d:\n%s", code, out.String())
	}
	if len(upstream.Messages()) != 6 {
		t.Errorf("upstream got %d events", len(upstream.Messages()))
	}
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", "")
	out.Reset()
	if code := run([]string{"--live"}, &out); code != 0 || !strings.Contains(out.String(), harness.Skipped) {
		t.Errorf("live without a key exited %d: %s", code, out.String())
	}
}

func TestRunErrors(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--nope"}, &out); code != 2 {
		t.Errorf("bad flag exit = %d", code)
	}
	t.Setenv("MCP_DEMO_TOKEN", "")
	if code := run(nil, &out); code != 1 || !strings.Contains(out.String(), "MCP_DEMO_TOKEN") {
		t.Errorf("missing token exit = %d: %s", code, out.String())
	}
	t.Setenv("MCP_DEMO_TOKEN", "demo")
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	if code := run(nil, &out); code != 1 || !strings.Contains(out.String(), "RUDDERSTACK_WRITE_KEY") {
		t.Errorf("missing env exit = %d: %s", code, out.String())
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "k")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", "http://127.0.0.1:1")
	if code := run([]string{"--addr=bad address"}, &out); code != 1 {
		t.Errorf("bad address exit = %d", code)
	}
}
