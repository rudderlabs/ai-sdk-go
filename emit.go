package rudderstackai

import (
	"log/slog"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

const (
	integrationMCP = "mcp"
	// systemAnonymousID is the anonymousId of events without a user.
	systemAnonymousID = "ai_system"
)

func debugf(msg string, args ...any) { slog.Debug("rudderstackai: "+msg, args...) }

// safely runs f and swallows its panic, so analytics never breaks the host server.
func safely(f func()) {
	defer func() {
		if r := recover(); r != nil {
			debugf("recovered from a panic in the integration", "panic", r)
		}
	}()
	f()
}

// who is the user an event is for.
type who struct {
	userID      string
	anonymousID string
}

// emit sends one track call. Never panics; a client error drops the event.
func emit(client analytics.Client, event string, w who, properties map[string]any) {
	safely(func() {
		msg := analytics.Track{
			Event:             event,
			UserId:            w.userID,
			AnonymousId:       w.anonymousID,
			Properties:        analytics.Properties(properties),
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
