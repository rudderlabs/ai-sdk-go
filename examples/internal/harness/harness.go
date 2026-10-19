// Package harness runs an example's end-to-end check, against a mock data plane or live: in
// live mode a local recorder forwards every batch to RUDDERSTACK_DATA_PLANE_URL and keeps only
// what it accepts. Every kept event is checked for the SDK's context and validated against
// spec/ai-events-v0.3.schema.json. It also serves the examples outside a check.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

// Skipped is what a live check without credentials prints.
const Skipped = "skipped: no key (set RUDDERSTACK_WRITE_KEY and RUDDERSTACK_DATA_PLANE_URL)"

// Sink receives an example's events.
type Sink struct {
	*dataplane.Server
	// WriteKey is the source write key: the real one when live.
	WriteKey string
}

// NewClient returns an analytics-go client that sends to the sink. Close it to flush.
func (s *Sink) NewClient() (analytics.Client, error) {
	return analytics.NewWithConfig(s.WriteKey, analytics.Config{DataPlaneUrl: s.URL, NoProxySupport: true, Interval: 20 * time.Millisecond})
}

// EnvClient is the RudderStack client from RUDDERSTACK_WRITE_KEY and
// RUDDERSTACK_DATA_PLANE_URL. Close it to flush.
func EnvClient() (analytics.Client, error) {
	key, url := os.Getenv("RUDDERSTACK_WRITE_KEY"), os.Getenv("RUDDERSTACK_DATA_PLANE_URL")
	if key == "" || url == "" {
		return nil, errors.New("set RUDDERSTACK_WRITE_KEY and RUDDERSTACK_DATA_PLANE_URL")
	}
	return analytics.NewWithConfig(key, analytics.Config{DataPlaneUrl: url, NoProxySupport: true})
}

// Serve serves h on addr until SIGINT or SIGTERM.
func Serve(addr string, h http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveUntil(ctx, addr, h)
}

func serveUntil(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Run runs check against a mock data plane, or live, then validates every event, and reports
// on stderr. It returns the exit code; a live run without credentials is skipped with 0.
func Run(stderr io.Writer, live bool, check func(*Sink) error) int {
	mode, sink := "check", &Sink{WriteKey: "check"}
	if live {
		key, url := os.Getenv("RUDDERSTACK_WRITE_KEY"), os.Getenv("RUDDERSTACK_DATA_PLANE_URL")
		if key == "" || url == "" {
			fmt.Fprintln(stderr, Skipped)
			return 0
		}
		mode, sink.WriteKey, sink.Server = "live check", key, dataplane.StartForwarding(url)
	} else {
		sink.Server = dataplane.Start()
	}
	defer sink.Close()
	err := check(sink)
	if err == nil {
		err = Validate(sink.Messages())
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s failed: %v\n", mode, err)
		return 1
	}
	fmt.Fprintf(stderr, "%s passed: %d events\n", mode, len(sink.Messages()))
	return 0
}

// Validate checks each event's context and its properties against the spec schema; declared
// tool events use mcp_declared_tool_event.
func Validate(msgs []dataplane.Message) error {
	if len(msgs) == 0 {
		return errors.New("no events")
	}
	c, err := compiler()
	if err != nil {
		return err
	}
	for _, m := range msgs {
		wl, _ := m.Context["wrapperLibrary"].(map[string]any)
		lib, _ := m.Context["library"].(map[string]any)
		if m.Type != "track" || wl["name"] != "ai-sdk-go" || wl["integration"] != "mcp" || wl["specVersion"] != "0.3.0" || lib["name"] != "analytics-go" {
			return fmt.Errorf("%s: type %q, context %v", m.Event, m.Type, m.Context)
		}
		def := m.Event
		if !strings.HasPrefix(def, "mcp_") {
			def = "mcp_declared_tool_event"
		}
		schema, err := c.Compile(schemaURL + "#/$defs/" + def)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Event, err)
		}
		b, _ := json.Marshal(m.Properties)
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err := schema.Validate(v); err != nil {
			return fmt.Errorf("%s: %w", m.Event, err)
		}
	}
	return nil
}

const (
	schemaURL  = "https://rudderstack.com/schemas/ai-events/0.3.0"
	schemaPath = "spec/ai-events-v0.3.schema.json"
)

// compiler loads the schema from the repository, found from the working directory up, so the
// examples run from the root (go run) and from their own directory (go test).
func compiler() (*jsonschema.Compiler, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		f, err := os.Open(filepath.Join(dir, schemaPath))
		if err == nil {
			defer f.Close()
			doc, err := jsonschema.UnmarshalJSON(f)
			if err != nil {
				return nil, err
			}
			c := jsonschema.NewCompiler()
			return c, c.AddResource(schemaURL, doc)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("%s not found; run from the repository", schemaPath)
		}
		dir = parent
	}
}

// ByEvent groups messages by event name.
func ByEvent(msgs []dataplane.Message) map[string][]dataplane.Message {
	out := map[string][]dataplane.Message{}
	for _, m := range msgs {
		out[m.Event] = append(out[m.Event], m)
	}
	return out
}

// Find returns the first message whose property key equals value. Batches can arrive out of
// order, so asserts find events by content rather than position.
func Find(msgs []dataplane.Message, key string, value any) map[string]any {
	want, _ := json.Marshal(value)
	for _, m := range msgs {
		if got, _ := json.Marshal(m.Properties[key]); bytes.Equal(got, want) {
			return m.Properties
		}
	}
	return map[string]any{}
}

// Counts checks how many of each event arrived.
func Counts(byEvent map[string][]dataplane.Message, want map[string]int) error {
	for name, n := range want {
		if len(byEvent[name]) != n {
			return fmt.Errorf("got %d %s, want %d", len(byEvent[name]), name, n)
		}
	}
	return nil
}

type marker int

const (
	// Absent wants the property left out.
	Absent marker = iota + 1
	// Present wants the property set, to any value.
	Present
)

// Fields checks properties field by field; values compare as JSON.
func Fields(event string, props map[string]any, want map[string]any) error {
	for k, w := range want {
		got, ok := props[k]
		switch w {
		case Absent:
			if ok {
				return fmt.Errorf("%s.%s = %v, want it left out", event, k, got)
			}
			continue
		case Present:
			if !ok {
				return fmt.Errorf("%s.%s missing", event, k)
			}
			continue
		}
		g, _ := json.Marshal(got)
		e, _ := json.Marshal(w)
		if !ok || !bytes.Equal(g, e) {
			return fmt.Errorf("%s.%s = %s, want %s", event, k, g, e)
		}
	}
	return nil
}
