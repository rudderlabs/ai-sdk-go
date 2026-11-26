package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

// PlausibleJSONNumbers reports whether JSON numbers stay below 1e15 in magnitude.
func PlausibleJSONNumbers(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		tok, err := d.Token()
		if err != nil {
			return err == io.EOF
		}
		if n, ok := tok.(json.Number); ok {
			if f, err := n.Float64(); err != nil || math.Abs(f) >= 1e15 {
				return false
			}
		}
	}
}

// JSONMap decodes a JSON object and fails the test on malformed input.
func JSONMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// JSONString marshals a value while ignoring encoding errors.
func JSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// PropertyMilliseconds reads a numeric event property as an integer duration.
func PropertyMilliseconds(t *testing.T, v any) time.Duration {
	t.Helper()
	var n float64
	if err := json.Unmarshal([]byte(fmt.Sprint(v)), &n); err != nil {
		t.Fatalf("not a number: %v", v)
	}
	return time.Duration(n) * time.Millisecond
}

// FixtureMilliseconds reads a required float64 fixture field as an integer duration.
func FixtureMilliseconds(t *testing.T, m map[string]any, key string) time.Duration {
	t.Helper()
	n, ok := m[key].(float64)
	if !ok {
		t.Fatalf("missing %s", key)
	}
	return time.Duration(n) * time.Millisecond
}

// BoolName returns the lower-case representation of a boolean.
func BoolName(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// Outcome returns the response text or a prefixed error message.
func Outcome(raw string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return raw
}

// MintedUUID reports whether an ID is a canonical version 4 UUID.
func MintedUUID(id any) bool {
	s, _ := id.(string)
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u.Version() == 4
}

// AppendTrack records a track under the recorder's lock.
func AppendTrack(mu *sync.Mutex, msgs *[]analytics.Track, m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected %T", m)
	}
	mu.Lock()
	*msgs = append(*msgs, t)
	mu.Unlock()
	return nil
}

// CheckRecordedEvents reports schema failures during test cleanup.
func CheckRecordedEvents(t *testing.T, mu *sync.Mutex, invalid *[]string) {
	t.Helper()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, msg := range *invalid {
			t.Errorf("event breaks the spec schema: %s", msg)
		}
	})
}

// SnapshotTracks copies a recorder's events while holding its lock.
func SnapshotTracks(mu *sync.Mutex, messages *[]analytics.Track) []analytics.Track {
	mu.Lock()
	defer mu.Unlock()
	return append([]analytics.Track(nil), (*messages)...)
}

// NamedTracks selects events with the requested name in recorded order.
func NamedTracks(messages []analytics.Track, event string) []analytics.Track {
	var out []analytics.Track
	for _, m := range messages {
		if m.Event == event {
			out = append(out, m)
		}
	}
	return out
}

// TrackNames returns the recorded event names in order.
func TrackNames(messages []analytics.Track) []string {
	var out []string
	for _, m := range messages {
		out = append(out, m.Event)
	}
	return out
}

// OnlyEventProperties requires exactly one event with the requested name.
func OnlyEventProperties(t *testing.T, event string, named func(string) []analytics.Track, names func() []string) map[string]any {
	t.Helper()
	got := named(event)
	if len(got) != 1 {
		t.Fatalf("want 1 %s, got %d: %v", event, len(got), names())
	}
	return got[0].Properties
}

// PanicWrite panics on the first write and accepts subsequent writes.
func PanicWrite(calls *int, p []byte) (int, error) {
	*calls++
	if *calls == 1 {
		panic("writer")
	}
	return len(p), nil
}

// RequireEventAtFlush requires the tool event to exist before analytics flush.
func RequireEventAtFlush(named func(string) []analytics.Track) error {
	if len(named("mcp_tool_called")) != 1 {
		return errors.New("event missing at analytics flush")
	}
	return nil
}

// UserAgentTransport supplies the same client identity to both MCP integrations.
type UserAgentTransport struct{}

func (UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", "Cursor/1.7.4")
	return http.DefaultTransport.RoundTrip(req)
}

// CaptureJSON exposes how many times the serializer was invoked.
func CaptureJSON(calls *int) ([]byte, error) {
	*calls++
	return json.Marshal(map[string]any{"public": fmt.Sprintf("capture-%d", *calls)})
}

// SlowJSON delays serialization like a large structured result.
func SlowJSON(delay time.Duration) ([]byte, error) {
	time.Sleep(delay)
	return []byte(`{"public":"visible"}`), nil
}

// RunInteraction runs a provider call with the supplied SDK runner and options.
// The type parameters let this package remain independent of the core SDK.
func RunInteraction[O, I any](t *testing.T, opts O, fn func(context.Context) error, run func(context.Context, O, func(context.Context, I) error) error) error {
	t.Helper()
	return run(context.Background(), opts, func(ctx context.Context, _ I) error { return fn(ctx) })
}
