package rudderstackai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://rudderstack.com/schemas/ai-events/0.3.0"

var (
	schemaOnce sync.Once
	schemas    map[string]*jsonschema.Schema
	schemaErr  error
)

var mcpEventDefs = []string{
	"mcp_client_seen", "mcp_tool_called", "mcp_elicitation_completed", "mcp_capability_requested",
	"mcp_resource_read", "mcp_prompt_used", "mcp_event_subscription_changed", "mcp_event_delivered",
	"mcp_declared_tool_event",
	"ai_user_prompt_created", "ai_response_received", "ai_llm_response_received",
	"ai_interaction_classified", "ai_interaction_scored", "ai_user_action", "declared_tool_event", "ai_task_started",
	"ai_limit_reached",
}

func loadSchemas() {
	f, err := os.Open("spec/ai-events-v0.3.schema.json")
	if err != nil {
		schemaErr = err
		return
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		schemaErr = err
		return
	}
	c := jsonschema.NewCompiler()
	if schemaErr = c.AddResource(schemaURL, doc); schemaErr != nil {
		return
	}
	schemas = map[string]*jsonschema.Schema{}
	for _, def := range mcpEventDefs {
		if schemas[def], schemaErr = c.Compile(schemaURL + "#/$defs/" + def); schemaErr != nil {
			return
		}
	}
}

// validateEvent checks one event's properties against its schema definition; customer-named
// events use declared_tool_event when they carry an interaction's conversation_id, else
// mcp_declared_tool_event.
func validateEvent(event string, props map[string]any) error {
	schemaOnce.Do(loadSchemas)
	if schemaErr != nil {
		return schemaErr
	}
	def := event
	if _, ok := schemas[def]; !ok {
		if strings.HasPrefix(event, "ai_") || strings.HasPrefix(event, "mcp_") {
			return fmt.Errorf("no schema for reserved event %q", event)
		}
		def = "mcp_declared_tool_event"
		if _, ai := props["conversation_id"]; ai {
			def = "declared_tool_event"
		}
	}
	b, err := json.Marshal(props)
	if err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return schemas[def].Validate(v)
}

// recorder is an analytics client that keeps every track call and validates it against the
// spec schema.
type recorder struct {
	mu       sync.Mutex
	messages []analytics.Track
	invalid  []string
}

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected message %T", m)
	}
	props := map[string]any(t.Properties)
	err := validateEvent(t.Event, props)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.invalid = append(r.invalid, fmt.Sprintf("%s: %v", t.Event, err))
	}
	r.messages = append(r.messages, t)
	return nil
}

func (r *recorder) Close() error { return nil }

func (r *recorder) all() []analytics.Track {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]analytics.Track(nil), r.messages...)
}

func (r *recorder) named(event string) []analytics.Track {
	var out []analytics.Track
	for _, m := range r.all() {
		if m.Event == event {
			out = append(out, m)
		}
	}
	return out
}

// props returns the properties of the only event with this name.
func (r *recorder) props(t *testing.T, event string) map[string]any {
	t.Helper()
	got := r.named(event)
	if len(got) != 1 {
		t.Fatalf("want 1 %s, got %d: %v", event, len(got), r.names())
	}
	return got[0].Properties
}

func (r *recorder) names() []string {
	var out []string
	for _, m := range r.all() {
		out = append(out, m.Event)
	}
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = nil
}

// wait polls until n events with this name arrived.
func (r *recorder) wait(t *testing.T, event string, n int) []analytics.Track {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.named(event)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d %s, got %d: %v", n, event, len(got), r.names())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newRecorder returns a recorder that fails the test on any event that breaks the schema.
func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, msg := range r.invalid {
			t.Errorf("event breaks the spec schema: %s", msg)
		}
	})
	return r
}

type listIn struct {
	WorkspaceID string `json:"workspace_id" jsonschema:"the workspace"`
	PageSize    int    `json:"page_size,omitempty"`
}

type listOut struct {
	Destinations []string `json:"destinations"`
	NextCursor   string   `json:"nextCursor,omitempty"`
}

type audienceIn struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

type audienceOut struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// handlerSeen records the arguments a tool handler received.
type handlerSeen struct {
	mu   sync.Mutex
	args []json.RawMessage
}

func (h *handlerSeen) add(raw json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.args = append(h.args, raw)
}

func (h *handlerSeen) last() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.args) == 0 {
		return ""
	}
	return string(h.args[len(h.args)-1])
}

// workspaceServer is a real go-sdk server with a paginated list tool, a create tool and a
// tool that fails.
func workspaceServer(name string, seen *handlerSeen) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.4.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_destinations", Description: "Lists destinations."},
		func(ctx context.Context, req *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
			if seen != nil {
				seen.add(req.Params.Arguments)
			}
			return nil, listOut{Destinations: []string{"secret-destination-" + in.WorkspaceID}, NextCursor: "c2"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "create_audience", Description: "Creates an audience.", InputSchema: map[string]any{
		"type":                 "object",
		"required":             []string{"name"},
		"additionalProperties": false,
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"type": map[string]any{"type": "string", "enum": []string{"computed", "static"}},
		},
	}},
		func(ctx context.Context, req *mcp.CallToolRequest, in audienceIn) (*mcp.CallToolResult, audienceOut, error) {
			return nil, audienceOut{ID: "aud_1", Type: in.Type}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fail", Description: "Always fails."},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, fmt.Errorf("boom with a secret")
		})
	return s
}

var testClientInfo = &mcp.Implementation{Name: "test-client", Title: "Test Client", Version: "1.2.3"}

// connect connects a real go-sdk client over in-memory transports.
func connect(t *testing.T, server *mcp.Server, protocol string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(testClientInfo, opts).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any, meta mcp.Meta) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func wrap(t *testing.T, server *mcp.Server, opts Options) *recorder {
	t.Helper()
	freshState(t)
	r := newRecorder(t)
	opts.Analytics = r
	if err := WrapMCPServer(server, opts); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
