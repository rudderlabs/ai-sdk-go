package ruddertest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	s := bufio.NewScanner(f)
	for s.Scan() {
		var m map[string]any
		if err := json.Unmarshal(s.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", s.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// The recorder is the app's own client: the app's legacy calls and the SDK's AI events land in one
// capture, each with its type, in the shape aicheck reads.
func TestRecorderCapturesTheSDKAndTheApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := ruddertest.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithAnalytics(nil)) })

	if err := rec.Enqueue(analytics.Track{UserId: "u1", Event: "chatTurnCompleted", Properties: analytics.NewProperties().Set("turns", 1)}); err != nil {
		t.Fatal(err)
	}
	_, in := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{
		UserID: "u1", ConversationID: "c1", Feature: "support_bot", Surface: rudderstackai.SurfaceWeb,
	})
	in.RecordCall(rudderstackai.LLMCall{Provider: "anthropic", Model: "claude-sonnet-4-5", InputTokens: 10, OutputTokens: 5})
	in.End(rudderstackai.EndOptions{})
	if !rudderstackai.Flush(context.Background()) {
		t.Fatal("Flush did not finish")
	}
	for _, m := range []analytics.Message{
		analytics.Identify{UserId: "u1"}, analytics.Page{UserId: "u1", Name: "Home"}, analytics.Screen{UserId: "u1", Name: "Home"},
		analytics.Group{UserId: "u1", GroupId: "g1"}, analytics.Alias{UserId: "u1", PreviousId: "a1"},
	} {
		if err := rec.Enqueue(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, path)
	var types, events []any
	for _, l := range lines {
		types = append(types, l["type"])
		if l["type"] == "track" {
			events = append(events, l["event"])
		}
	}
	wantTypes := []any{"track", "track", "track", "identify", "page", "screen", "group", "alias"}
	if len(types) != len(wantTypes) {
		t.Fatalf("types %v, want %v", types, wantTypes)
	}
	for i := range wantTypes {
		if types[i] != wantTypes[i] {
			t.Fatalf("types %v, want %v", types, wantTypes)
		}
	}
	wantEvents := []any{"chatTurnCompleted", "ai_user_prompt_created", "ai_response_received"}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Fatalf("events %v, want %v", events, wantEvents)
		}
	}
	// aicheck reads properties, userId and context.wrapperLibrary of each track call.
	answer := lines[2]
	props, _ := answer["properties"].(map[string]any)
	w, _ := answer["context"].(map[string]any)["wrapperLibrary"].(map[string]any)
	if answer["userId"] != "u1" || props["interaction_id"] == nil || props["response_status"] != "success" ||
		w["name"] != "ai-sdk-go" || w["specVersion"] != rudderstackai.SpecVersion {
		t.Fatalf("answer line %v", answer)
	}
	if legacy := lines[0]; legacy["context"] != nil && legacy["context"].(map[string]any)["wrapperLibrary"] != nil {
		t.Fatalf("legacy line carries a wrapperLibrary: %v", legacy)
	}
}

type custom struct{ analytics.Track }

func TestRecorderOtherMessagesAndErrors(t *testing.T) {
	if _, err := ruddertest.NewRecorder(filepath.Join(t.TempDir(), "missing", "events.jsonl")); err == nil {
		t.Fatal("NewRecorder in a missing directory: no error")
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := ruddertest.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Enqueue(custom{}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []analytics.Message{
		&analytics.Track{Event: "e"}, &analytics.Identify{}, &analytics.Page{}, &analytics.Screen{}, &analytics.Group{}, &analytics.Alias{},
		(*analytics.Track)(nil), (*analytics.Identify)(nil), (*analytics.Page)(nil), (*analytics.Screen)(nil), (*analytics.Group)(nil), (*analytics.Alias)(nil),
	} {
		if err := rec.Enqueue(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Enqueue(analytics.Track{Event: "x", Properties: analytics.Properties{"bad": make(chan int)}}); err == nil {
		t.Fatal("unencodable message: no error")
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Enqueue(analytics.Track{Event: "late"}); err == nil {
		t.Fatal("Enqueue after Close: no error")
	}
	lines := readLines(t, path)
	var types []any
	for _, l := range lines {
		types = append(types, l["type"])
	}
	want := []any{"ruddertest_test.custom", "track", "identify", "page", "screen", "group", "alias",
		"*analytics.Track", "*analytics.Identify", "*analytics.Page", "*analytics.Screen", "*analytics.Group", "*analytics.Alias"}
	if fmt.Sprint(types) != fmt.Sprint(want) || lines[1]["event"] != "e" {
		t.Fatalf("lines %v", lines)
	}
}
