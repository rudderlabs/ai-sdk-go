// Package ruddertest records what an app sends to RudderStack, for the aicheck migration check
// (go install github.com/rudderlabs/ai-sdk-go/cmd/aicheck@latest).
//
// Inject the [Recorder] as the app's own analytics-go client, where the app builds it, so the app's
// legacy events and the AI SDK's events land in one capture, in the order they were sent:
//
//	client, err := ruddertest.NewRecorder("events.jsonl") // instead of analytics.NewWithConfig(...)
//	if err != nil { ... }
//	defer client.Close()
//	// the app's setup as usual: rudderstackai.Configure(rudderstackai.WithAnalytics(client)), its switches, ...
//
// Configuring a recorder after the app has started records only the AI events sent from then on,
// not the app's own calls through its original client.
package ruddertest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// Recorder is an analytics.Client that appends every message to a JSONL file, one JSON object per
// line with its type (track, identify, page, screen, group, alias). It sends nothing. A message of
// another type is recorded as {"type": "<its Go type>"}. Safe for concurrent use.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
}

var _ analytics.Client = (*Recorder)(nil)

// NewRecorder opens path for appending, creating it if needed.
func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Recorder{f: f}, nil
}

// Enqueue writes m as one line; a pointer to a message is recorded like the message. analytics-go sets a message's type only when it sends it, so the
// recorder sets it.
func (r *Recorder) Enqueue(m analytics.Message) error {
	var line any
	switch msg := deref(m).(type) {
	case analytics.Track:
		msg.Type = "track"
		line = msg
	case analytics.Identify:
		msg.Type = "identify"
		line = msg
	case analytics.Page:
		msg.Type = "page"
		line = msg
	case analytics.Screen:
		msg.Type = "screen"
		line = msg
	case analytics.Group:
		msg.Type = "group"
		line = msg
	case analytics.Alias:
		msg.Type = "alias"
		line = msg
	default:
		line = map[string]string{"type": fmt.Sprintf("%T", m)}
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.f.Write(append(b, '\n'))
	return err
}

// deref returns the message a non-nil pointer message points to, so it is recorded like a value;
// anything else, a nil pointer included, is returned as is.
func deref(m analytics.Message) analytics.Message {
	switch msg := m.(type) {
	case *analytics.Track:
		if msg != nil {
			return *msg
		}
	case *analytics.Identify:
		if msg != nil {
			return *msg
		}
	case *analytics.Page:
		if msg != nil {
			return *msg
		}
	case *analytics.Screen:
		if msg != nil {
			return *msg
		}
	case *analytics.Group:
		if msg != nil {
			return *msg
		}
	case *analytics.Alias:
		if msg != nil {
			return *msg
		}
	}
	return m
}

// Flush makes rudderstackai.Flush treat the recorder as a current client; writes are unbuffered,
// so there is nothing to send.
func (r *Recorder) Flush(context.Context) error { return nil }

// Close closes the file. Messages enqueued after Close return an error.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
