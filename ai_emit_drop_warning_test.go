package rudderstackai

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEmitDropWarningsDistinguishSerializationAndSize(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	t.Setenv("RUDDERSTACK_AI_DEBUG", "")
	logs := &logBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	client := &sink{}
	event := "ai_response_received"
	// An oversized ID forces encoding instead of the size-bound fast path.
	for range 2 {
		emitAIRates(client, event, strings.Repeat("x", 40000), "", nil, nil, IntegrationManual,
			time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC), nil)
		emitAIRates(client, event, strings.Repeat("x", 40000), "", nil, nil, IntegrationManual,
			time.Now(), nil)
	}
	for _, message := range []string{
		event + " could not be serialized; dropped it",
		event + " was over 32 KiB after trimming; dropped it",
	} {
		if count := strings.Count(logs.String(), message); count != 1 {
			t.Fatalf("warning %q occurred %d times; logs: %s", message, count, logs)
		}
	}
	if events := client.named(event); len(events) != 0 {
		t.Fatalf("dropped events reached client: %v", events)
	}
}
