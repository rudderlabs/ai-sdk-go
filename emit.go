package rudderstackai

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

const (
	// logPrefix starts every log line, as in Node.js and Python.
	logPrefix      = "[rudderstack-ai] "
	integrationMCP = "mcp"
	// systemAnonymousID is the anonymousId of events without a user.
	systemAnonymousID = "ai_system"
)

// debugf logs what tracking drops or decides at debug level. RUDDERSTACK_AI_DEBUG set to any
// non-empty value (read on every call, as in Node.js and Python) writes it even when the slog
// handler's level is above debug.
func debugf(msg string, args ...any) {
	// Errors by type only: their text may carry request credentials or headers.
	for k, a := range args {
		if err, ok := a.(error); ok {
			args[k] = fmt.Sprintf("%T", err)
		}
	}
	ctx := context.Background()
	h := slog.Default().Handler()
	if h.Enabled(ctx, slog.LevelDebug) || os.Getenv("RUDDERSTACK_AI_DEBUG") == "" {
		slog.Debug(logPrefix+msg, args...)
		return
	}
	r := slog.NewRecord(time.Now(), slog.LevelDebug, logPrefix+msg, 0)
	r.Add(args...)
	_ = h.Handle(ctx, r)
}

// safely runs f and swallows its panic, so analytics never breaks the host server.
func safely(f func()) {
	defer func() {
		if r := recover(); r != nil {
			report(KindInternal, callerOf("safely"), recovered(r))
		}
	}()
	f()
}

// who is the user an event is for.
type who struct {
	userID      string
	anonymousID string
}

// emit sends one MCP track call, within the properties budget, unless WithSampleRates samples it
// out. Never panics; a client error drops the event.
func emit(client analytics.Client, event string, w who, properties map[string]any) {
	emitRates(client, event, w, properties, cfg().sampleRates)
}

// emitRates is emit under one rates snapshot, so an event fitted under it is sampled under it.
func emitRates(client analytics.Client, event string, w who, properties map[string]any, rates map[string]float64) {
	safely(func() {
		rememberClient(client)
		properties, kept := sample(event, properties, rates)
		if !kept {
			return
		}
		msg := analytics.Track{
			Event:             event,
			UserId:            w.userID,
			AnonymousId:       w.anonymousID,
			Properties:        analytics.Properties(fitProperties(event, jsonSafeMap(properties))),
			OriginalTimestamp: time.Now().UTC(),
			Context: &analytics.Context{Extra: map[string]any{
				"wrapperLibrary": map[string]any{
					"name":        LibraryName,
					"version":     Version,
					"specVersion": SpecVersion,
					"integration": integrationMCP,
				},
			}},
		}
		if msg.UserId == "" && msg.AnonymousId == "" {
			msg.AnonymousId = systemAnonymousID
		}
		if err := client.Enqueue(msg); err != nil {
			debugf("track failed; event dropped", "event", event, "error", err)
		}
	})
}

// put sets key when value is non-empty.
func put(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}
