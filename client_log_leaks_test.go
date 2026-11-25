package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

type clientLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *clientLog) Logf(format string, args ...any)   { l.add(format, args...) }
func (l *clientLog) Errorf(format string, args ...any) { l.add(format, args...) }
func (l *clientLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

// Shared with the Node.js and Python SDKs: the RudderStack client's own log never shows an app value.
func TestAnOversizedEventNeverReachesTheClientLog(t *testing.T) {
	const secret = "my order 8812 never arrived"
	ctx := context.Background()
	cases := []struct {
		name, event, outcome string // outcome: sent (trimmed) or dropped
		run                  func()
	}{
		{"action_details", "ai_user_action", "sent", func() { // the details go, the action stays
			TrackAction(ctx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: "i", ConversationID: "c", ActionDetails: map[string]any{"notes": strings.Repeat(secret, 1500)}})
		}},
		{"context", "ai_response_received", "sent", func() { // without the app's context
			_, i := StartInteraction(ctx, InteractionOptions{UserID: "u", Context: map[string]any{"traits": map[string]any{"bio": strings.Repeat(secret, 1500)}}})
			i.End(EndOptions{})
		}},
		{"user ID", "ai_user_action", "dropped", func() {
			TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: "i", ConversationID: "c", UserID: strings.Repeat(secret, 1300)})
		}},
		{"HTML-heavy prompt text", "ai_user_prompt_created", "sent", func() { // small until the client escapes it
			_, i := StartInteraction(ctx, InteractionOptions{UserID: "u", PromptText: strings.Repeat("<", 6000) + secret})
			i.End(EndOptions{})
		}},
		{"invalid json.Number", "ai_user_action", "sent", func() { // without the number
			TrackAction(ctx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: "i", ConversationID: "c", ActionDetails: map[string]any{"feedback_value": json.Number("4.5 stars"), "feedback_text": secret}})
		}},
		{"text that fits once the context goes", "ai_response_received", "sent", func() { // keeps its text
			_, i := StartInteraction(ctx, InteractionOptions{UserID: "u", Context: map[string]any{"traits": map[string]any{"bio": strings.Repeat(secret, 1200)}}})
			i.End(EndOptions{ResponseText: strings.Repeat("a", 8000)})
		}},
		{"prompt text and context", "ai_user_prompt_created", "sent", func() { // without prompt_text
			_, i := StartInteraction(ctx, InteractionOptions{UserID: "u", PromptText: strings.Repeat(secret, 1100), Context: map[string]any{"traits": map[string]any{"plan": strings.Repeat(secret, 120)}}})
			i.End(EndOptions{})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dp := dataplane.Start()
			defer dp.Close()
			log := &clientLog{}
			client, err := analytics.NewWithConfig("write-key", analytics.Config{DataPlaneUrl: dp.URL, BatchSize: 1, NoProxySupport: true, Interval: 10 * time.Millisecond, Logger: log})
			if err != nil {
				t.Fatal(err)
			}
			setupAI(t, WithAnalytics(client), WithCaptureContent(true))
			logs := captureLogs(t)
			t.Setenv("RUDDERSTACK_AI_DEBUG", "1")
			c.run()
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			if out := log.b.String() + logs.String(); strings.Contains(out, "8812") {
				t.Fatalf("logged an app value: %.300s", out)
			}
			var sent []string
			for _, m := range dp.Messages() {
				sent = append(sent, m.Event)
				if c.name == "text that fits once the context goes" && m.Event == c.event && m.Properties["response_text"] == nil {
					t.Fatal("response_text trimmed although it fits without the context")
				}
			}
			switch {
			case c.outcome == "sent" && !strings.Contains(strings.Join(sent, ","), c.event):
				t.Fatalf("%s not sent: %v", c.event, sent)
			case c.outcome == "dropped" && logs.count(c.event+" was over 32 KiB after trimming; dropped it") == 0:
				t.Fatalf("no drop warning: %s", logs)
			}
		})
	}
}

// quotedBytes may overcount (Go 1.27 writes invalid UTF-8 as a raw U+FFFD); it must never undercount.
func FuzzQuotedBytesBoundsMarshal(f *testing.F) {
	for _, s := range []string{"", "plain", `q"b\s`, "<a>&</a>", "\n\t\x01\x7f", "界  ", "\xff\xfe bad", "🙂"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if b, _ := json.Marshal(s); quotedBytes(s) < len(b) {
			t.Fatalf("quotedBytes(%q) = %d, below json.Marshal's %d", s, quotedBytes(s), len(b))
		}
	})
}

func TestBoundBytesIsNeverBelowWhatTheClientMeasures(t *testing.T) {
	nils := map[string]any{}
	for i := range 40 {
		nils[fmt.Sprint(i)] = []string(nil)
		nils[fmt.Sprint("m", i)] = map[string]any(nil)
	}
	tricky := strings.Repeat("<&> 界\xff\"\\\n", 50)
	raw, _ := encodeNow(struct{ A string }{tricky})
	for name, msg := range map[string]analytics.Track{
		"empty":          {OriginalTimestamp: time.Now(), Properties: analytics.Properties{}, Context: &analytics.Context{Extra: map[string]any{}}},
		"nil containers": {OriginalTimestamp: time.Now(), Properties: analytics.Properties(jsonSafeMap(nils)), Context: &analytics.Context{Extra: map[string]any{}}},
		"tricky": {Event: tricky, UserId: tricky, OriginalTimestamp: time.Now(),
			Properties: analytics.Properties(jsonSafeMap(map[string]any{
				"s": tricky, "n": -1.2345678901234567e-308, "i": int64(-1 << 63), "b": true, "nil": nil,
				"list": []any{tricky, 1, map[string]any{tricky: []string{tricky}}}, "maps": []map[string]any{{"k": tricky}},
				"num": json.Number("12345"), "raw": raw,
				"nilStrings": []string(nil), "nilList": []any(nil), "nilMaps": []map[string]any(nil), "nilMap": map[string]any(nil),
			})),
			Context: &analytics.Context{Extra: map[string]any{"traits": map[string]any{tricky: tricky}, "wrapperLibrary": map[string]any{"name": "x"}}}},
	} {
		bound, ok := boundBytes(msg)
		if exact := clientBytes(msg); !ok || bound < exact {
			t.Errorf("%s: bound %d (ok %v) below the client's %d", name, bound, ok, exact)
		}
	}
}
