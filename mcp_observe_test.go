package rudderstackai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// connectMaybeObserved connects a client over pipes, the server side an IOTransport wrapped
// with ObserveMCPTransport when observe is set.
func connectMaybeObserved(t *testing.T, server *mcp.Server, observe bool) *mcp.ClientSession {
	t.Helper()
	c1, c2 := net.Pipe()
	return connectOver(t, server, &mcp.IOTransport{Reader: c2, Writer: c2}, &mcp.IOTransport{Reader: c1, Writer: c1}, observe)
}

func connectOver(t *testing.T, server *mcp.Server, st, ct mcp.Transport, observe bool) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	if observe {
		ctx, st = ObserveMCPTransport(ctx, st)
	}
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(testClientInfo, nil).Connect(context.Background(), ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
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

// failingWriter fails every write of an error response.
type failingWriter struct{ net.Conn }

func (w failingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"error"`) {
		return 0, errors.New("write failed")
	}
	return w.Conn.Write(p)
}

func TestObserveMCPTransportFailedWriteSendsWithoutMessage(t *testing.T) {
	setupAI(t)
	s := failingServer("failed-write-mcp")
	r := wrap(t, s, Options{CaptureContent: true})
	c1, c2 := net.Pipe()
	cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: failingWriter{c2}}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
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

// Observing keeps go-sdk's own connection, so a JSON-RPC batch is still refused after the
// session negotiates 2025-06-18.
func TestObserveMCPTransportKeepsBatchRefusal(t *testing.T) {
	for _, observe := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "observed"}[observe], func(t *testing.T) {
			setupAI(t)
			s := failingServer("batch-mcp")
			wrap(t, s, Options{CaptureContent: true})
			client, server := net.Pipe()
			ctx := context.Background()
			var st mcp.Transport = &mcp.IOTransport{Reader: server, Writer: server}
			if observe {
				ctx, st = ObserveMCPTransport(ctx, st)
			}
			ss, err := s.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close(); _ = client.Close() })
			lines := bufio.NewScanner(client)
			send := func(msg string) {
				t.Helper()
				_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, err := io.WriteString(client, msg+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if !lines.Scan() || !strings.Contains(lines.Text(), `"protocolVersion":"2025-06-18"`) {
				t.Fatalf("initialize: %q %v", lines.Text(), lines.Err())
			}
			send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			send(`[{"jsonrpc":"2.0","id":2,"method":"ping"},{"jsonrpc":"2.0","id":3,"method":"ping"}]`)
			_ = client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			if lines.Scan() {
				t.Fatalf("batch answered: %q", lines.Text())
			}
		})
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

func TestObserveMCPTransportUnsupportedUnchanged(t *testing.T) {
	ctx := context.Background()
	_, inMemory := mcp.NewInMemoryTransports()
	for _, tr := range []mcp.Transport{nil, inMemory, &mcp.StreamableServerTransport{}, (*mcp.IOTransport)(nil), &mcp.IOTransport{}} {
		gotCtx, got := ObserveMCPTransport(ctx, tr)
		if got != tr || gotCtx != ctx {
			t.Errorf("%T changed", tr)
		}
	}
	stdio := &mcp.StdioTransport{MaxLineLength: 42}
	_, got := ObserveMCPTransport(ctx, stdio)
	if io, ok := got.(*mcp.IOTransport); !ok || io.MaxLineLength != 42 {
		t.Errorf("stdio: %#v", got)
	}
}

// A flush (response, close or timeout) racing a later event of the same request still sends
// the held event first.
func TestHeldEventsKeepOrderDuringFlush(t *testing.T) {
	setupAI(t)
	s := workspaceServer("order-mcp", nil)
	wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	responses := mcpbridge.NewResponses()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: int64(9), Responses: responses})
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}
	started, release := make(chan struct{}), make(chan struct{})
	rc.send("mcp_tool_called", map[string]any{"tool_status": "error", "error_type": "api_error"}, func() {
		close(started)
		<-release
		record("mcp_tool_called")
	})
	go responses.Close()
	<-started
	done := make(chan struct{})
	go func() {
		defer close(done)
		rc.send("lookup_failed", map[string]any{"tool_status": "error"}, func() { record("lookup_failed") })
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "mcp_tool_called" {
		t.Errorf("order: %v", order)
	}
}
