package main

import (
	"bytes"
	"testing"
)

func TestCheck(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--check"}, &out); code != 0 {
		t.Fatalf("check exited %d: %s", code, out.String())
	}
}
