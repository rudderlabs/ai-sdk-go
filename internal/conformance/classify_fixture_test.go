package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/openai/openai-go/v3"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type fixtureCodedError string

func (e fixtureCodedError) Error() string     { return string(e) }
func (e fixtureCodedError) ErrorCode() string { return string(e) }

// Build native Go error equivalents; foreign runtime class fields are not Go error APIs.
func nativeFixtureError(e obj) error {
	for _, key := range []string{"cause", "lastError"} {
		if child, ok := e[key].(obj); ok {
			return fmt.Errorf("fixture wrapper: %w", nativeFixtureError(child))
		}
	}
	switch str(e["name"]) {
	case "ThrottlingException", "AccessDeniedException":
		return fixtureCodedError(str(e["name"]))
	case "TimeoutError":
		return context.DeadlineExceeded
	case "AbortError", "CancelledError":
		return context.Canceled
	case "APIConnectionError":
		return &net.OpError{Op: "dial", Err: errors.New("fixture")}
	}
	body := obj0(e["body"])
	nested := obj0(body["error"])
	if len(nested) == 0 {
		nested = obj0(e["error"])
	}
	code, message := str(e["code"]), str(e["message"])
	if v := str(nested["code"]); v != "" {
		code = v
	}
	if v := str(nested["message"]); v != "" {
		message = v
	} else if v := str(body["message"]); v != "" {
		message = v
	}
	if num(e["status"]) > 0 || str(e["name"]) == "APIError" || code == "insufficient_quota" || code == "rate_limit_exceeded" || len(nested) > 0 || len(body) > 0 {
		return &openai.Error{StatusCode: int(num(e["status"])), Code: code, Message: message}
	}
	return errors.New("fixture application error")
}
func TestSharedClassifyErrorFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testkit.RepoRoot(), "spec/fixtures/classify-errors-v0.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string
			Error    obj
			Expected string
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			switch str(c.Error["name"]) {
			case "MaxTurnsExceededError", "LengthFinishReasonError", "ContentFilterFinishReasonError":
				t.Skip("Go has no equivalent Agent SDK or response-parser exception class")
			}
			rec := &recorder{}
			configure(t, obj{}, rec)
			original := nativeFixtureError(c.Error)
			err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Classify: rudderstackai.Ptr(false)}, func(context.Context, *rudderstackai.Interaction) error { return original })
			if err != original {
				t.Fatal("error changed")
			}
			found := false
			for _, m := range rec.all() {
				if m.Event == "ai_response_received" {
					found = true
					if m.Properties["error_type"] != c.Expected {
						t.Fatalf("got %v want %s", m.Properties["error_type"], c.Expected)
					}
				}
			}
			if !found {
				t.Fatal("missing response")
			}
		})
	}
}
