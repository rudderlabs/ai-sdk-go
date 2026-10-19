package ruddermcpgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://rudderstack.com/schemas/ai-events/0.3.0"

var (
	schemaOnce sync.Once
	compiler   *jsonschema.Compiler
	schemaErr  error
)

// validateEvent checks one event's properties against its v0.3 schema definition;
// customer-named events use mcp_declared_tool_event.
func validateEvent(event string, props map[string]any) error {
	schemaOnce.Do(func() {
		f, err := os.Open("../spec/ai-events-v0.3.schema.json")
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
		compiler = jsonschema.NewCompiler()
		schemaErr = compiler.AddResource(schemaURL, doc)
	})
	if schemaErr != nil {
		return schemaErr
	}
	def := event
	if !strings.HasPrefix(event, "mcp_") {
		def = "mcp_declared_tool_event"
	}
	schema, err := compiler.Compile(schemaURL + "#/$defs/" + def)
	if err != nil {
		return err
	}
	b, err := json.Marshal(props)
	if err != nil {
		return err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return schema.Validate(v)
}

// recorder keeps every track call and fails the test on one that breaks the schema.
type recorder struct {
	mu       sync.Mutex
	messages []analytics.Track
	invalid  []string
}

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

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected message %T", m)
	}
	err := validateEvent(t.Event, t.Properties)
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

func (r *recorder) names() []string {
	var out []string
	for _, m := range r.all() {
		out = append(out, m.Event)
	}
	return out
}

func (r *recorder) props(t *testing.T, event string) map[string]any {
	t.Helper()
	got := r.named(event)
	if len(got) != 1 {
		t.Fatalf("want 1 %s, got %d: %v", event, len(got), r.names())
	}
	return got[0].Properties
}

func (r *recorder) find(t *testing.T, event, key string, value any) map[string]any {
	t.Helper()
	for _, m := range r.named(event) {
		if m.Properties[key] == value {
			return m.Properties
		}
	}
	t.Fatalf("no %s with %s=%v: %v", event, key, value, r.names())
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// handlerSeen records the arguments a tool handler received.
type handlerSeen struct {
	mu   sync.Mutex
	args []map[string]any
}

func (h *handlerSeen) add(args map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.args = append(h.args, args)
}

func (h *handlerSeen) last() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.args) == 0 {
		return nil
	}
	return h.args[len(h.args)-1]
}

// workspaceServer is a real mcp-go server with tools, a resource, a template and a prompt.
func workspaceServer(name string, seen *handlerSeen) *server.MCPServer {
	s := server.NewMCPServer(name, "1.4.0", server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, false), server.WithPromptCapabilities(false), server.WithRecovery())
	s.AddTool(mcp.NewTool("list_destinations", mcp.WithDescription("Lists destinations."),
		mcp.WithString("workspace_id", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if seen != nil {
				seen.add(req.GetArguments())
			}
			res := mcp.NewToolResultText(`{"destinations":["secret-destination"],"nextCursor":"c2"}`)
			res.StructuredContent = map[string]any{"destinations": []any{"secret-destination"}, "nextCursor": "c2"}
			return res, nil
		})
	s.AddTool(mcp.NewTool("create_audience", mcp.WithDescription("Creates an audience."),
		mcp.WithString("name", mcp.Required()), mcp.WithString("type", mcp.Enum("computed", "static"))),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.GetString("name", "") == "" {
				return mcp.NewToolResultError("name is required"), nil
			}
			return mcp.NewToolResultText(`{"id":"aud_1","type":"computed"}`), nil
		})
	s.AddTool(mcp.NewTool("fail", mcp.WithDescription("Always fails.")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, fmt.Errorf("boom with a secret")
		})
	s.AddResource(mcp.NewResource("docs://readme", "readme"), func(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: req.Params.URI, Text: "private body"}}, nil
	})
	s.AddResourceTemplate(mcp.NewResourceTemplate("destinations://{destination_id}", "destination"),
		func(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: req.Params.URI, Text: "private body"}}, nil
		})
	s.AddPrompt(mcp.NewPrompt("audit_tracking_plan"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return mcp.NewGetPromptResult("audit", []mcp.PromptMessage{mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("private prompt"))}), nil
	})
	return s
}

var testClient = &sdk.Implementation{Name: "test-client", Title: "Test Client", Version: "1.2.3"}

func wrap(t *testing.T, s *server.MCPServer, opts Options) *recorder {
	t.Helper()
	r := newRecorder(t)
	opts.Analytics = r
	if err := WrapServer(s, opts); err != nil {
		t.Fatal(err)
	}
	return r
}

// connectStdio serves s over the stdio protocol on pipes and connects a real go-sdk client.
func connectStdio(t *testing.T, s *server.MCPServer, protocol string, opts *sdk.ClientOptions) *sdk.ClientSession {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.NewStdioServer(s).Listen(ctx, serverIn, serverOut)
	}()
	cs, err := sdk.NewClient(testClient, opts).Connect(context.Background(),
		&sdk.IOTransport{Reader: clientIn, Writer: clientOut}, &sdk.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = serverIn.Close()
		_ = clientIn.Close()
		<-done
	})
	return cs
}

// connectHTTP serves s over streamable HTTP and connects a real go-sdk client.
func connectHTTP(t *testing.T, s *server.MCPServer, protocol string, opts *sdk.ClientOptions, httpOpts ...server.StreamableHTTPOption) *sdk.ClientSession {
	t.Helper()
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s, httpOpts...))
	t.Cleanup(ts.Close)
	cs, err := sdk.NewClient(testClient, opts).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, &sdk.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any, meta sdk.Meta) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}
