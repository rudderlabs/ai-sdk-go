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

type reusedResponseWriter struct {
	net.Conn
	started chan struct{}
	release chan struct{}
}

func (w reusedResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "first failure") {
		close(w.started)
		<-w.release
	}
	return w.Conn.Write(p)
}

func TestObserveMCPTransportReusedResponseSnapshot(t *testing.T) {
	for _, source := range []string{"rpc", "result"} {
		t.Run(source, func(t *testing.T) {
			setupAI(t)
			text := &mcp.TextContent{Text: "first failure"}
			result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{text}}
			rpc := &jsonrpc.Error{Code: -32602, Message: "first failure"}
			handled := make(chan string, 2)
			s := mcp.NewServer(&mcp.Implementation{Name: "reuse", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				handled <- ctx.Value(reqCtxKey{}).(*reqCtx).requestID
				if source == "rpc" {
					return nil, rpc
				}
				return result, nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			c1, c2 := net.Pipe()
			w := reusedResponseWriter{Conn: c2, started: make(chan struct{}), release: make(chan struct{})}
			cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: w}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
			release := sync.OnceFunc(func() { close(w.release) })
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			}()
			select {
			case <-w.started:
			case <-ctx.Done():
				t.Fatal("first write never started")
			}
			if len(r.named("mcp_tool_called")) != 0 {
				t.Fatal("event before successful write")
			}
			// The first response is serialized; reuse its source while that write is held.
			firstID := <-handled
			text.Text, rpc.Message = "second failure", "second failure"
			second := make(chan struct{})
			go func() {
				defer close(second)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			}()
			var secondID string
			select {
			case secondID = <-handled:
			case <-ctx.Done():
				t.Fatal("second request never handled")
			}
			release()
			<-done
			<-second
			events := r.wait(t, "mcp_tool_called", 2)
			seen := map[any]any{}
			for _, e := range events {
				seen[e.Properties["request_id"]] = e.Properties["error_message"]
			}
			if len(seen) != 2 || seen[firstID] != "first failure" || seen[secondID] != "second failure" {
				t.Fatalf("request messages: %v", seen)
			}
		})
	}
}

// The function field cannot be serialized without MarshalJSON, like Node's toJSON case.
type snapshotErrorData struct{ Unserializable func() }

func (snapshotErrorData) MarshalJSON() ([]byte, error) {
	return []byte(`{"detail":"custom data"}`), nil
}

type snapshotResponseWriter struct {
	reusedResponseWriter
	fail bool
}

func (w snapshotResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "first failure") && w.fail {
		close(w.started)
		<-w.release
		return 0, errors.New("write failed")
	}
	return w.reusedResponseWriter.Write(p)
}

func TestObserveMCPTransportCustomMarshalerSnapshot(t *testing.T) {
	for _, source := range []string{"rpc", "result"} {
		for _, fail := range []bool{false, true} {
			t.Run(source+map[bool]string{false: "/written", true: "/write-failed"}[fail], func(t *testing.T) {
				setupAI(t)
				// go-sdk requires RawMessage for error data, so applications marshal it first.
				data, err := json.Marshal(snapshotErrorData{Unserializable: func() {}})
				if err != nil {
					t.Fatal(err)
				}
				s := mcp.NewServer(&mcp.Implementation{Name: "custom-data", Version: "1"}, nil)
				s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if source == "rpc" {
						return nil, &jsonrpc.Error{Code: -32602, Message: "first failure", Data: data}
					}
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
						&mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"},
						&mcp.TextContent{Text: "first failure"},
					}}, nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
				c1, c2 := net.Pipe()
				w := snapshotResponseWriter{reusedResponseWriter: reusedResponseWriter{Conn: c2, started: make(chan struct{}), release: make(chan struct{})}, fail: fail}
				cs := connectOver(t, s, &mcp.IOTransport{Reader: c2, Writer: w}, &mcp.IOTransport{Reader: c1, Writer: c1}, true)
				release := sync.OnceFunc(func() { close(w.release) })
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
					if err == nil && (result == nil || !result.IsError) {
						err = errors.New("expected isError result")
					}
					done <- err
				}()
				select {
				case <-w.started:
				case <-ctx.Done():
					t.Fatal("response write never started")
				}
				if len(r.named("mcp_tool_called")) != 0 {
					t.Fatal("event before write completed")
				}
				release()
				callErr := <-done
				if fail || source == "rpc" {
					if callErr == nil {
						t.Fatal("expected call error")
					}
				} else if callErr != nil {
					t.Fatal(callErr)
				}
				if !fail && source == "rpc" {
					var rpc *jsonrpc.Error
					if !errors.As(callErr, &rpc) || string(rpc.Data) != string(data) {
						t.Fatalf("custom data missing from wire error: %v", callErr)
					}
				}
				r.wait(t, "mcp_tool_called", 1)
				_ = cs.Close() // Closing observation must not emit the held event again.
				events := r.named("mcp_tool_called")
				if len(events) != 1 {
					t.Fatalf("got %d tool events, want exactly one", len(events))
				}
				p := events[0].Properties
				if p["tool_status"] != "error" {
					t.Fatalf("status: %v", p)
				}
				message, present := p["error_message"]
				if fail && present || !fail && (!present || message != "first failure") {
					t.Fatalf("error_message after write (failed=%v): %v", fail, p)
				}
			})
		}
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
			Write         string
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
			out := io.Discard
			if tc.Write == "fails" {
				out = fixtureFailingWriter{}
			}
			_, _ = mcpbridge.NewWriter(out, responses).Write(append(b, '\n'))
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

func TestObserveMCPStdioFailedWriteSendsWithoutMessage(t *testing.T) {
	setupAI(t)
	s := workspaceServer("stdio-write-mcp", nil)
	r := wrap(t, s, Options{CaptureContent: true})
	w, _ := instances.get(s)
	closed, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	saved := os.Stdout
	os.Stdout = closed
	ctx, transport := ObserveMCPTransport(context.Background(), &mcp.StdioTransport{})
	os.Stdout = saved
	responses := mcpbridge.ResponsesFrom(ctx)
	rc := w.begin(mcpbridge.Request{Ctx: ctx, RequestID: int64(1), Responses: responses})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	rc.ToolDone(mcpbridge.Outcome{ErrorType: "api_error"})
	if len(r.named("mcp_tool_called")) != 0 {
		t.Fatal("sent before write")
	}
	_, err = transport.(*mcp.IOTransport).Writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"never written"}}` + "\n"))
	if err == nil {
		t.Fatal("expected closed-file write error")
	}
	if _, ok := r.props(t, "mcp_tool_called")["error_message"]; ok {
		t.Fatal("text from failed stdio write")
	}
}
