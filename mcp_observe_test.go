package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// connectMaybeObserved connects a client over in-memory transports, the server side wrapped
// with ObserveMCPTransport when observe is set.
func connectMaybeObserved(t *testing.T, server *mcp.Server, observe bool) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	var transport mcp.Transport = st
	if observe {
		transport = ObserveMCPTransport(st)
	}
	return connectOver(t, server, transport, ct)
}

func connectOver(t *testing.T, server *mcp.Server, st, ct mcp.Transport) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(testClientInfo, nil).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func failingServer(name string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return nil, &jsonrpc.Error{Code: -32602, Message: args.Text}
	})
	return s
}

func TestObserveMCPTransportConcurrentIDs(t *testing.T) {
	setupAI(t)
	s := failingServer("concurrent-errors-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	cs := connectMaybeObserved(t, s, true)
	var wg sync.WaitGroup
	for _, text := range []string{"first failure", "second failure", "third failure", "fourth failure"} {
		wg.Go(func() {
			if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": text}}); err == nil {
				t.Error("expected error")
			}
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, e := range r.wait(t, "mcp_tool_called", 4) {
		seen[e.Properties["error_message"].(string)] = true
	}
	if len(seen) != 4 {
		t.Errorf("messages not matched by id: %v", seen)
	}
}

func TestObserveMCPTransportDeclaredEventFollows(t *testing.T) {
	setupAI(t)
	s := failingServer("declared-errors-mcp")
	r := wrap(t, s, Options{CaptureContent: true, EventRules: EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_failed"}}}})
	cs := connectMaybeObserved(t, s, true)
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "boom"}}); err == nil {
		t.Fatal("expected error")
	}
	r.wait(t, "lookup_failed", 1)
	names := r.names()
	if len(names) < 2 || names[len(names)-2] != "mcp_tool_called" || names[len(names)-1] != "lookup_failed" {
		t.Errorf("order: %v", names)
	}
	if r.props(t, "mcp_tool_called")["error_message"] != "boom" {
		t.Errorf("props: %v", r.props(t, "mcp_tool_called"))
	}
}

// failingConn fails every error response write.
type failingConn struct{ mcp.Connection }

func (c failingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if r, ok := msg.(*jsonrpc.Response); ok && r.Error != nil {
		return errors.New("write failed")
	}
	return c.Connection.Write(ctx, msg)
}

type failingTransport struct{ mcp.Transport }

func (t failingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	return failingConn{c}, err
}

func TestObserveMCPTransportFailedWriteSendsWithoutMessage(t *testing.T) {
	setupAI(t)
	s := failingServer("failed-write-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	ct, st := mcp.NewInMemoryTransports()
	cs := connectOver(t, s, ObserveMCPTransport(failingTransport{st}), ct)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "never sent"}}); err == nil {
		t.Fatal("expected the call to fail")
	}
	p := r.wait(t, "mcp_tool_called", 1)[0].Properties
	if p["tool_status"] != "error" {
		t.Errorf("status: %v", p)
	}
	if _, ok := p["error_message"]; ok {
		t.Errorf("error_message without a delivered response: %v", p)
	}
}

func TestObserveMCPTransportCloseFlushes(t *testing.T) {
	setupAI(t)
	s := workspaceServer("close-errors-mcp", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: int64(3), Responses: responses})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
	if len(r.named("mcp_tool_called")) != 0 {
		t.Fatal("sent before the response")
	}
	responses.Close()
	if _, ok := r.props(t, "mcp_tool_called")["error_message"]; ok {
		t.Error("error_message without a response")
	}
}

// The response a held event receives yields the fixture's expected text.
func TestObserveMCPTransportFixtureParity(t *testing.T) {
	data, err := os.ReadFile("spec/fixtures/mcp-error-message-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string
			Result, Error map[string]any
			Expected      *string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			setupAI(t)
			s := workspaceServer("fixture-errors-mcp", nil)
			r := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			responses := mcpbridge.NewResponses()
			rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: "fixture", Responses: responses})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
			msg := map[string]any{"jsonrpc": "2.0", "id": "fixture"}
			if tc.Error != nil {
				msg["error"] = tc.Error
			} else {
				msg["result"] = tc.Result
			}
			b, _ := json.Marshal(msg)
			responses.DeliverJSON(b)
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got, _ := r.props(t, "mcp_tool_called")["error_message"].(string); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestObserveMCPTransportNilAndVersions(t *testing.T) {
	if ObserveMCPTransport(nil) != nil {
		t.Error("nil transport wrapped")
	}
	st := &mcp.StreamableServerTransport{}
	v, ok := ObserveMCPTransport(st).(mcp.ProtocolVersionSupporter)
	if !ok || v.SupportsProtocolVersion("2026-07-28") != st.SupportsProtocolVersion("2026-07-28") {
		t.Error("protocol versions not forwarded")
	}
	_, inner := mcp.NewInMemoryTransports()
	if !ObserveMCPTransport(inner).(mcp.ProtocolVersionSupporter).SupportsProtocolVersion("2025-06-18") {
		t.Error("transport without a supporter should support every version")
	}
}
