package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

func TestCheckerAcceptsEmittedFeedback(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "capture"}[capture], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feedback.jsonl")
			recorder, err := ruddertest.NewRecorder(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recorder.Close() })
			s := mcp.NewServer(&mcp.Implementation{Name: "checker-feedback", Version: "1"}, nil)
			if err := rudderstackai.WrapMCPServer(s, rudderstackai.Options{Analytics: recorder, CaptureContent: capture, FeedbackRequests: &rudderstackai.FeedbackRequestOptions{}}); err != nil {
				t.Fatal(err)
			}
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			ss, err := s.Connect(context.Background(), serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close() })
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "checker", Version: "1"}, nil).Connect(context.Background(), clientTransport, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			args := map[string]any{"category": "feature_request", "capability": "unknown"}
			if capture {
				args["feedback"] = "email ann@example.com"
			}
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "submit_feedback", Arguments: args})
			if err != nil || res.IsError {
				t.Fatalf("feedback result=%v err=%v", res, err)
			}
			flags := []string{path, "--mcp-anonymous", "--mcp-server", "checker-feedback"}
			if capture {
				flags = append(flags, "--capture-content")
			}
			var stdout, stderr bytes.Buffer
			if code := run(flags, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "RESULT: PASS") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if capture {
				stdout.Reset()
				stderr.Reset()
				if code := run(flags[:len(flags)-1], &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "content sent with capture off: ['feedback_text']") {
					t.Fatalf("capture leak check: exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
				}
			}
		})
	}
}
